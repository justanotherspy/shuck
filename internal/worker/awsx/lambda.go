package awsx

import (
	"context"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

// SQSEventHandler adapts the worker core to a Lambda SQS event source. Each
// record's body goes through handle (worker.Processor.ProcessMessage); a
// failed record is reported as a batch item failure so SQS redelivers only
// it, never the whole batch.
//
// The event source mapping MUST enable ReportBatchItemFailures (JUS-92):
// without it Lambda ignores the response body, a nil error deletes the
// entire batch, and failed records would be silently lost.
func SQSEventHandler(handle func(ctx context.Context, body []byte) error, log *slog.Logger,
) func(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
		var resp events.SQSEventResponse
		for _, rec := range event.Records {
			if err := handle(ctx, []byte(rec.Body)); err != nil {
				log.Warn("record failed; reporting batch item failure",
					"message_id", rec.MessageId, "err", err)
				resp.BatchItemFailures = append(resp.BatchItemFailures,
					events.SQSBatchItemFailure{ItemIdentifier: rec.MessageId})
			}
		}
		// Always a nil error: a non-nil error would fail (and redeliver)
		// the whole batch, including the records that succeeded.
		return resp, nil
	}
}
