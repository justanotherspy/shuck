// Command shuck-ingest is the webhook ingest of shuck's opt-in self-hosted
// mode (JUS-86): it verifies GitHub webhook deliveries, dedupes them,
// filters to the events workers care about, and enqueues slim envelopes to
// SQS. It runs either as a plain HTTP server or as a Lambda behind a
// function URL — the same handler backs both, chosen by auto-detecting the
// Lambda runtime.
//
// Configuration is environment-only (deploy tooling injects secrets;
// JUS-92/93 own that wiring):
//
//	SHUCK_WEBHOOK_SECRET  GitHub App webhook secret (required)
//	SHUCK_QUEUE_URL       SQS queue URL for envelopes (required)
//	SHUCK_DEDUPE_TABLE    DynamoDB dedupe table name (required)
//	SHUCK_DEDUPE_TTL      dedupe row retention (default 1h)
//	SHUCK_ADDR            HTTP listen address (default :8080; server mode)
//
// This binary is part of the self-hosted backend only. The portable shuck
// CLI does not link it and is unaffected by it (see docs/V2.md).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/justanotherspy/shuck/internal/ingest"
	"github.com/justanotherspy/shuck/internal/ingest/awsx"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), log); err != nil {
		log.Error("shuck-ingest failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	secret := os.Getenv("SHUCK_WEBHOOK_SECRET")
	queueURL := os.Getenv("SHUCK_QUEUE_URL")
	table := os.Getenv("SHUCK_DEDUPE_TABLE")
	if secret == "" || queueURL == "" || table == "" {
		return fmt.Errorf("SHUCK_WEBHOOK_SECRET, SHUCK_QUEUE_URL, and SHUCK_DEDUPE_TABLE are required")
	}
	ttl := time.Hour
	if v := os.Getenv("SHUCK_DEDUPE_TTL"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse SHUCK_DEDUPE_TTL: %w", err)
		}
		ttl = parsed
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	handler := &ingest.Handler{
		Secret:  []byte(secret),
		Dedupe:  awsx.NewDynamoDeduper(dynamodb.NewFromConfig(awsCfg), table, ttl),
		Queue:   awsx.NewSQSEnqueuer(sqs.NewFromConfig(awsCfg), queueURL),
		Subs:    ingest.AllowAll{}, // tighten once the JUS-88 subscription table exists
		Log:     log,
		Metrics: &ingest.Metrics{},
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		log.Info("starting in Lambda mode")
		lambda.StartWithOptions(awsx.FunctionURLHandler(mux), lambda.WithContext(ctx))
		return nil
	}
	addr := os.Getenv("SHUCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Info("starting HTTP server", "addr", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return server.ListenAndServe()
}
