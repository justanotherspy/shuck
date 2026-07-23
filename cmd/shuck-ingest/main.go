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
//	SHUCK_WEBHOOK_SECRET       GitHub App webhook secret (required)
//	SHUCK_QUEUE_URL            SQS queue URL for envelopes (required)
//	SHUCK_DEDUPE_TABLE         DynamoDB dedupe table name (required)
//	SHUCK_DEDUPE_TTL           dedupe row retention (default 1h; must be > 0)
//	SHUCK_SUBSCRIPTION_TABLE   gateway subscription table for the pre-filter
//	                           (optional; unset means every event is enqueued)
//	SHUCK_ADDR                 HTTP listen address (default :8080; server mode)
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
	"github.com/justanotherspy/shuck/internal/lambdahttp"
	"github.com/justanotherspy/shuck/internal/promexpo"
)

// version is stamped at build time via -X main.version (Makefile /
// Dockerfile.backend); untagged builds report "dev".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	log.Info("shuck-ingest starting", "version", version)
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
		parsed, err := parseTTL(v)
		if err != nil {
			return err
		}
		ttl = parsed
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	ddb := dynamodb.NewFromConfig(awsCfg)
	var subs ingest.SubscriptionChecker = ingest.AllowAll{}
	if subTable := os.Getenv("SHUCK_SUBSCRIPTION_TABLE"); subTable != "" {
		subs = awsx.NewDynamoSubscriptionChecker(ddb, subTable)
	}
	metrics := &ingest.Metrics{}
	handler := &ingest.Handler{
		Secret:  []byte(secret),
		Dedupe:  awsx.NewDynamoDeduper(ddb, table, ttl),
		Queue:   awsx.NewSQSEnqueuer(sqs.NewFromConfig(awsCfg), queueURL),
		Subs:    subs,
		Log:     log,
		Metrics: metrics,
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		log.Info("starting in Lambda mode")
		lambda.StartWithOptions(lambdahttp.FunctionURLHandler(mux), lambda.WithContext(ctx))
		return nil
	}
	addr := os.Getenv("SHUCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// Opt-in Prometheus metrics on a dedicated listener (server mode only;
	// Lambda mode is scraped via CloudWatch). No-op unless SHUCK_METRICS_ADDR
	// is set.
	go func() {
		if err := promexpo.Serve(ctx, os.Getenv(promexpo.EnvAddr), log, metrics.Snapshot); err != nil {
			log.Error("metrics listener failed", "err", err)
		}
	}()
	// Server mode also logs a periodic counter snapshot (the worker
	// precedent) so a plain log pipeline sees the delivery funnel without
	// scraping; the ticker stops when run returns.
	metricsCtx, stopMetrics := context.WithCancel(ctx)
	defer stopMetrics()
	go logMetrics(metricsCtx, log, metrics, time.Minute)
	log.Info("starting HTTP server", "addr", addr)
	return newServer(addr, mux).ListenAndServe()
}

// parseTTL validates SHUCK_DEDUPE_TTL. A zero or negative retention would
// make every dedupe row expire immediately, silently disabling redelivery
// protection, so it is a configuration error.
func parseTTL(v string) (time.Duration, error) {
	ttl, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse SHUCK_DEDUPE_TTL: %w", err)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("SHUCK_DEDUPE_TTL must be positive, got %s", ttl)
	}
	return ttl, nil
}

// newServer wraps the mux with the public-endpoint timeouts.
// ReadHeaderTimeout alone leaves a body-slowloris (headers complete, body
// dripped byte by byte) holding a connection open forever, so the whole
// read and idle keep-alives are bounded too.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// logMetrics snapshots the ingest counters periodically so a plain log
// pipeline sees the delivery funnel the runbook watches (received →
// verified → enqueued, plus every drop bucket); mirrors cmd/shuck-worker.
func logMetrics(ctx context.Context, log *slog.Logger, m *ingest.Metrics, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Info("metrics",
				"received", m.Received.Load(),
				"verified", m.Verified.Load(),
				"deduped", m.Deduped.Load(),
				"dropped", m.Dropped.Load(),
				"unsubscribed", m.Unsubscribed.Load(),
				"enqueued", m.Enqueued.Load(),
				"errors", m.Errors.Load(),
			)
		}
	}
}
