package awsx

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// fakeSQS scripts the narrowed SQS client per call and records deletes and
// the receive batch sizes requested.
type fakeSQS struct {
	mu       sync.Mutex
	receives []func() (*sqs.ReceiveMessageOutput, error)
	calls    int
	batches  []int32  // MaxNumberOfMessages of each receive
	deleted  []string // receipt handles
	delErr   error
	cancel   context.CancelFunc // invoked when the script is exhausted
}

func (f *fakeSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, in.MaxNumberOfMessages)
	if f.calls >= len(f.receives) {
		f.cancel()
		return nil, ctx.Err()
	}
	step := f.receives[f.calls]
	f.calls++
	return step()
}

func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, aws.ToString(in.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, f.delErr
}

func msg(id, body string) types.Message {
	return types.Message{
		MessageId:     aws.String(id),
		Body:          aws.String(body),
		ReceiptHandle: aws.String("rh-" + id),
	}
}

// runConsumer drives c.Run until the fake's script is exhausted.
func runConsumer(t *testing.T, f *fakeSQS, handle func(context.Context, []byte) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.cancel = cancel
	c := &Consumer{Client: f, QueueURL: "q", Handle: handle,
		Log: slog.New(slog.DiscardHandler), ErrPause: time.Millisecond}
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want the context's error", err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("consumer hung until the test deadline")
	}
}

func TestConsumerDeletesOnSuccessOnly(t *testing.T) {
	f := &fakeSQS{receives: []func() (*sqs.ReceiveMessageOutput, error){
		func() (*sqs.ReceiveMessageOutput, error) {
			return &sqs.ReceiveMessageOutput{Messages: []types.Message{msg("a", "ok"), msg("b", "boom")}}, nil
		},
	}}
	var handled []string
	runConsumer(t, f, func(_ context.Context, body []byte) error {
		handled = append(handled, string(body))
		if string(body) == "boom" {
			return errors.New("processing failed")
		}
		return nil
	})

	if strings.Join(handled, ",") != "ok,boom" {
		t.Errorf("handled %v", handled)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "rh-a" {
		t.Errorf("deleted %v, want only the succeeded message", f.deleted)
	}
}

func TestConsumerSurvivesReceiveError(t *testing.T) {
	f := &fakeSQS{receives: []func() (*sqs.ReceiveMessageOutput, error){
		func() (*sqs.ReceiveMessageOutput, error) { return nil, errors.New("sqs down") },
		func() (*sqs.ReceiveMessageOutput, error) {
			return &sqs.ReceiveMessageOutput{Messages: []types.Message{msg("a", "ok")}}, nil
		},
	}}
	runConsumer(t, f, func(context.Context, []byte) error { return nil })

	if f.calls != 2 {
		t.Errorf("receive called %d times, want the loop to survive the error", f.calls)
	}
	if len(f.deleted) != 1 {
		t.Errorf("message after the error not processed: deleted=%v", f.deleted)
	}
}

func TestConsumerDeleteErrorIsNonFatal(t *testing.T) {
	f := &fakeSQS{
		delErr: errors.New("receipt expired"),
		receives: []func() (*sqs.ReceiveMessageOutput, error){
			func() (*sqs.ReceiveMessageOutput, error) {
				return &sqs.ReceiveMessageOutput{Messages: []types.Message{msg("a", "ok"), msg("b", "ok")}}, nil
			},
		},
	}
	runConsumer(t, f, func(context.Context, []byte) error { return nil })

	if len(f.deleted) != 2 {
		t.Errorf("a delete failure must not stop the batch: deleted=%v", f.deleted)
	}
}

func TestConsumerStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Consumer{Client: &fakeSQS{}, QueueURL: "q", Handle: func(context.Context, []byte) error { return nil }}
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

func TestConsumerDefaults(t *testing.T) {
	c := &Consumer{}
	if c.waitSeconds() != 20 || c.batch() != DefaultBatch {
		t.Errorf("defaults: wait=%v batch=%d", c.waitSeconds(), c.batch())
	}
}

func TestConsumerBatchClamped(t *testing.T) {
	// SQS rejects MaxNumberOfMessages > 10; an unclamped Batch would fail
	// every receive and warn-pause the loop forever.
	tests := []struct {
		batch int32
		want  int32
	}{
		{0, DefaultBatch},
		{-1, DefaultBatch},
		{5, 5},
		{10, 10},
		{11, 10},
		{25, 10},
	}
	for _, tt := range tests {
		c := &Consumer{Batch: tt.batch, Log: slog.New(slog.DiscardHandler)}
		if got := c.batch(); got != tt.want {
			t.Errorf("batch(%d) = %d, want %d", tt.batch, got, tt.want)
		}
	}

	// End to end: the clamped value reaches the ReceiveMessage request.
	f := &fakeSQS{receives: []func() (*sqs.ReceiveMessageOutput, error){
		func() (*sqs.ReceiveMessageOutput, error) { return &sqs.ReceiveMessageOutput{}, nil },
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.cancel = cancel
	c := &Consumer{Client: f, QueueURL: "q", Batch: 25,
		Handle: func(context.Context, []byte) error { return nil },
		Log:    slog.New(slog.DiscardHandler)}
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want the context's error", err)
	}
	for _, got := range f.batches {
		if got != 10 {
			t.Errorf("receive requested %d messages, want the SQS max 10", got)
		}
	}
}
