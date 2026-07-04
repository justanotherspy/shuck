package awsx

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestSQSEventHandlerPartialBatch(t *testing.T) {
	handler := SQSEventHandler(func(_ context.Context, body []byte) error {
		if string(body) == "boom" {
			return errors.New("poison")
		}
		return nil
	}, slog.New(slog.DiscardHandler))

	resp, err := handler(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "m1", Body: "ok"},
		{MessageId: "m2", Body: "boom"},
		{MessageId: "m3", Body: "ok"},
		{MessageId: "m4", Body: "boom"},
	}})
	if err != nil {
		t.Fatalf("handler must never return an error (it would fail the whole batch): %v", err)
	}
	if len(resp.BatchItemFailures) != 2 ||
		resp.BatchItemFailures[0].ItemIdentifier != "m2" ||
		resp.BatchItemFailures[1].ItemIdentifier != "m4" {
		t.Errorf("batch item failures = %+v, want exactly m2 and m4", resp.BatchItemFailures)
	}
}

func TestSQSEventHandlerAllOK(t *testing.T) {
	handler := SQSEventHandler(func(context.Context, []byte) error { return nil }, nil)
	resp, err := handler(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "m1", Body: "ok"},
	}})
	if err != nil || len(resp.BatchItemFailures) != 0 {
		t.Fatalf("resp=%+v err=%v, want a clean response", resp, err)
	}
}
