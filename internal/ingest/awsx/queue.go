package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/justanotherspy/shuck/internal/ingest"
)

// SQSAPI is the subset of the SQS client the enqueuer uses.
type SQSAPI interface {
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// SQSEnqueuer implements ingest.Enqueuer by sending envelopes to one SQS
// queue. The envelope kind is mirrored into a message attribute so
// consumers and DLQ tooling can route without decoding the body.
type SQSEnqueuer struct {
	client   SQSAPI
	queueURL string
}

// NewSQSEnqueuer returns an enqueuer targeting queueURL.
func NewSQSEnqueuer(client SQSAPI, queueURL string) *SQSEnqueuer {
	return &SQSEnqueuer{client: client, queueURL: queueURL}
}

// Enqueue encodes env and sends it.
func (q *SQSEnqueuer) Enqueue(ctx context.Context, env ingest.Envelope) error {
	body, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode envelope %s: %w", env.DeliveryID, err)
	}
	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.queueURL),
		MessageBody: aws.String(string(body)),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"kind": {
				DataType:    aws.String("String"),
				StringValue: aws.String(string(env.Kind)),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("send envelope %s: %w", env.DeliveryID, err)
	}
	return nil
}
