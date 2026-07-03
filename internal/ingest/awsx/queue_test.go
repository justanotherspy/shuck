package awsx

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/justanotherspy/shuck/internal/ingest"
)

type fakeSQS struct {
	err  error
	sent []*sqs.SendMessageInput
}

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.sent = append(f.sent, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sqs.SendMessageOutput{}, nil
}

func TestSQSEnqueuer(t *testing.T) {
	fake := &fakeSQS{}
	q := NewSQSEnqueuer(fake, "https://sqs.example/queue")
	env := ingest.Envelope{
		Schema:     ingest.EnvelopeSchema,
		DeliveryID: "d-1",
		Kind:       ingest.KindCIFailure,
		Repo:       "octo/repo",
		PR:         9,
		RunID:      1234,
	}
	if err := q.Enqueue(t.Context(), env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(fake.sent))
	}
	in := fake.sent[0]
	if *in.QueueUrl != "https://sqs.example/queue" {
		t.Fatalf("queue = %q", *in.QueueUrl)
	}
	got, err := ingest.ParseEnvelope([]byte(*in.MessageBody))
	if err != nil {
		t.Fatalf("message body does not parse as an envelope: %v", err)
	}
	if got != env {
		t.Fatalf("wire envelope = %+v, want %+v", got, env)
	}
	if kind := *in.MessageAttributes["kind"].StringValue; kind != "ci_failure" {
		t.Fatalf("kind attribute = %q", kind)
	}
}

func TestSQSEnqueuerError(t *testing.T) {
	q := NewSQSEnqueuer(&fakeSQS{err: errors.New("sqs down")}, "url")
	env := ingest.Envelope{Schema: ingest.EnvelopeSchema, DeliveryID: "d", Kind: ingest.KindPRClosed, Repo: "o/r", PR: 1}
	if err := q.Enqueue(t.Context(), env); err == nil {
		t.Fatal("expected the send error to propagate")
	}
}
