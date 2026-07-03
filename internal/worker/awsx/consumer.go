// Package awsx contains the worker's AWS adapters: the SQS poll-loop
// consumer, the Lambda SQS-event entrypoint, and the S3 raw-log store. It
// is the only worker package that imports AWS SDKs, keeping the core
// (internal/worker) pure and the portable shuck binary free of them.
package awsx

import (
	"context"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Poll-loop defaults: SQS's maximum long-poll wait and receive batch, and a
// short pause after a receive error so a broken queue doesn't spin the pod.
const (
	DefaultWaitTime = 20 * time.Second
	DefaultBatch    = 10
	DefaultErrPause = 5 * time.Second
)

// SQSAPI is the subset of the SQS client the consumer uses.
type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// Consumer long-polls one SQS queue and hands each message body to Handle
// (worker.Processor.ProcessMessage). A nil Handle result deletes the
// message; an error leaves it for the visibility timeout to redeliver, so
// persistent failures reach the DLQ via the queue's redrive policy — the
// consumer itself never drops work. This is the container-mode entrypoint;
// Lambda deployments use SQSEventHandler instead.
type Consumer struct {
	Client   SQSAPI
	QueueURL string
	Handle   func(ctx context.Context, body []byte) error
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// WaitTime is the long-poll wait; 0 means DefaultWaitTime.
	WaitTime time.Duration
	// Batch is the max messages per receive; 0 means DefaultBatch.
	Batch int32
	// ErrPause is the sleep after a receive error; 0 means DefaultErrPause.
	ErrPause time.Duration
}

// Run polls until ctx is done (its error is then returned). Receive errors
// are logged and retried — a transient SQS failure must not kill the
// worker.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := c.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.QueueURL),
			MaxNumberOfMessages: c.batch(),
			WaitTimeSeconds:     c.waitSeconds(),
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log().Warn("receive failed", "queue", c.QueueURL, "err", err)
			c.pause(ctx)
			continue
		}

		for _, msg := range out.Messages {
			if err := c.Handle(ctx, []byte(aws.ToString(msg.Body))); err != nil {
				// Not deleted: redelivered after the visibility timeout,
				// DLQ'd by the redrive policy when it keeps failing.
				c.log().Warn("message failed; leaving for redelivery",
					"message_id", aws.ToString(msg.MessageId), "err", err)
				continue
			}
			if _, err := c.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(c.QueueURL),
				ReceiptHandle: msg.ReceiptHandle,
			}); err != nil {
				// The message redelivers and the gateway's event_id dedupe
				// absorbs the duplicate — log, don't fail.
				c.log().Warn("delete failed; message will redeliver",
					"message_id", aws.ToString(msg.MessageId), "err", err)
			}
		}
	}
}

// pause sleeps ErrPause or until ctx is cancelled.
func (c *Consumer) pause(ctx context.Context) {
	pause := c.ErrPause
	if pause <= 0 {
		pause = DefaultErrPause
	}
	t := time.NewTimer(pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// waitSeconds converts WaitTime to SQS's long-poll parameter, clamped to
// the API's valid [1, 20] second range.
func (c *Consumer) waitSeconds() int32 {
	wait := c.WaitTime
	if wait <= 0 {
		wait = DefaultWaitTime
	}
	secs := wait / time.Second
	switch {
	case secs < 1:
		return 1
	case secs > 20:
		return 20
	default:
		return int32(secs)
	}
}

func (c *Consumer) batch() int32 {
	if c.Batch <= 0 {
		return DefaultBatch
	}
	return c.Batch
}

func (c *Consumer) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}
