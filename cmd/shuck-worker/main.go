// Command shuck-worker is the event worker of shuck's opt-in self-hosted
// mode (JUS-87, JUS-91): it consumes queue envelopes, mints GitHub App
// installation tokens, fetches a failed run's jobs and logs — or a review
// comment / submitted review with its context — runs the shared parser,
// and delivers the capped summary to the gateway.
// It runs either as an SQS long-poll loop (container mode) or as a Lambda
// behind an SQS event source mapping — the same core backs both, chosen by
// auto-detecting the Lambda runtime. The Lambda event source mapping must
// enable ReportBatchItemFailures (JUS-92).
//
// Configuration is environment-only (deploy tooling injects secrets;
// JUS-92/93 own that wiring):
//
//	SHUCK_GITHUB_APP_ID              GitHub App ID (required)
//	SHUCK_GITHUB_APP_PRIVATE_KEY     App private key PEM (this or _FILE required)
//	SHUCK_GITHUB_APP_PRIVATE_KEY_FILE  path to the PEM (k8s secret mounts)
//	SHUCK_DELIVER_URL                gateway /internal/deliver URL (required)
//	SHUCK_DELIVER_SECRET             deliver shared secret (required)
//	SHUCK_QUEUE_URL                  SQS queue URL (required in poll mode)
//	SHUCK_RAW_LOG_BUCKET             S3 bucket for raw job logs (optional;
//	                                 unset disables archiving)
//	SHUCK_SUMMARY_LIMIT              summary byte cap (default 16384)
//	SHUCK_IGNORE_AUTHORS             comma-separated bot identities (numeric
//	                                 GitHub user IDs and/or logins) whose
//	                                 review events are dropped (optional;
//	                                 the bot-loop guard)
//	SHUCK_REVIEW_CONTEXT_LINES       file lines around a review comment
//	                                 (default 10)
//	SHUCK_GITHUB_API_URL             GitHub API base (optional; GHES)
//	SHUCK_ADDR                       healthz listen address (default :8080;
//	                                 poll mode)
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
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/justanotherspy/shuck/internal/worker"
	"github.com/justanotherspy/shuck/internal/worker/awsx"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), log); err != nil {
		log.Error("shuck-worker failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	appID, err := parseAppID(os.Getenv("SHUCK_GITHUB_APP_ID"))
	if err != nil {
		return err
	}
	keyPEM, err := loadPrivateKey()
	if err != nil {
		return err
	}
	deliverURL := os.Getenv("SHUCK_DELIVER_URL")
	deliverSecret := os.Getenv("SHUCK_DELIVER_SECRET")
	if deliverURL == "" || deliverSecret == "" {
		return fmt.Errorf("SHUCK_DELIVER_URL and SHUCK_DELIVER_SECRET are required")
	}
	summaryLimit := 0 // the Processor turns zero into distil.DefaultSummaryLimit
	if v := os.Getenv("SHUCK_SUMMARY_LIMIT"); v != "" {
		if summaryLimit, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("parse SHUCK_SUMMARY_LIMIT: %w", err)
		}
	}

	tokens, err := worker.NewAppTokenSource(appID, keyPEM)
	if err != nil {
		return err
	}
	metrics := &worker.Metrics{}
	tokens.Metrics = metrics
	apiBase := os.Getenv("SHUCK_GITHUB_API_URL")
	if apiBase != "" {
		tokens.BaseURL = apiBase
	}

	contextLines := 0 // the Processor turns zero into distil.DefaultContextLines
	if v := os.Getenv("SHUCK_REVIEW_CONTEXT_LINES"); v != "" {
		if contextLines, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("parse SHUCK_REVIEW_CONTEXT_LINES: %w", err)
		}
	}

	fetch := &worker.GHFetcher{APIBase: apiBase, Log: log}
	processor := &worker.Processor{
		Tokens:        tokens,
		Fetch:         fetch,
		Reviews:       fetch,
		Deliver:       &worker.HTTPDeliverer{URL: deliverURL, Secret: deliverSecret, Log: log, Metrics: metrics},
		SummaryLimit:  summaryLimit,
		IgnoreAuthors: worker.ParseIgnoreAuthors(os.Getenv("SHUCK_IGNORE_AUTHORS")),
		ContextLines:  contextLines,
		Log:           log,
		Metrics:       metrics,
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	if bucket := os.Getenv("SHUCK_RAW_LOG_BUCKET"); bucket != "" {
		processor.Logs = awsx.NewS3LogStore(s3.NewFromConfig(awsCfg), bucket)
	}

	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		log.Info("starting in Lambda mode")
		lambda.StartWithOptions(awsx.SQSEventHandler(processor.ProcessMessage, log), lambda.WithContext(ctx))
		return nil
	}

	queueURL := os.Getenv("SHUCK_QUEUE_URL")
	if queueURL == "" {
		return fmt.Errorf("SHUCK_QUEUE_URL is required in poll mode")
	}
	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go serveHealthz(runCtx, log)
	go logMetrics(runCtx, log, metrics)

	consumer := &awsx.Consumer{
		Client:   sqs.NewFromConfig(awsCfg),
		QueueURL: queueURL,
		Handle:   processor.ProcessMessage,
		Log:      log,
	}
	log.Info("starting SQS poll loop", "queue", queueURL)
	if err := consumer.Run(runCtx); err != nil && runCtx.Err() == nil {
		return err
	}
	log.Info("shutting down")
	return nil
}

// parseAppID validates the required numeric App ID.
func parseAppID(v string) (int64, error) {
	if v == "" {
		return 0, fmt.Errorf("SHUCK_GITHUB_APP_ID is required")
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse SHUCK_GITHUB_APP_ID: %w", err)
	}
	return id, nil
}

// loadPrivateKey reads the App private key from the env var or the mounted
// file, preferring the inline value when both are set.
func loadPrivateKey() ([]byte, error) {
	if v := os.Getenv("SHUCK_GITHUB_APP_PRIVATE_KEY"); v != "" {
		return []byte(v), nil
	}
	if path := os.Getenv("SHUCK_GITHUB_APP_PRIVATE_KEY_FILE"); path != "" {
		key, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("read SHUCK_GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
		return key, nil
	}
	return nil, fmt.Errorf("SHUCK_GITHUB_APP_PRIVATE_KEY or SHUCK_GITHUB_APP_PRIVATE_KEY_FILE is required")
}

// serveHealthz runs the tiny liveness endpoint for poll mode (k8s probes;
// JUS-93). Lambda mode has no listener.
func serveHealthz(ctx context.Context, log *slog.Logger) {
	addr := os.Getenv("SHUCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		// Detached from ctx (already done) but keeps its values.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Warn("healthz server failed", "err", err)
	}
}

// logMetrics snapshots the counters periodically so a plain log pipeline
// sees throughput, latency, truncation rate, and the shared GitHub quota.
func logMetrics(ctx context.Context, log *slog.Logger, m *worker.Metrics) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Info("metrics",
				"received", m.Received.Load(),
				"invalid", m.Invalid.Load(),
				"pr_closed", m.PRClosed.Load(),
				"review_comments", m.ReviewComments.Load(),
				"reviews", m.Reviews.Load(),
				"bot_dropped", m.BotDropped.Load(),
				"dup_skipped", m.DupSkipped.Load(),
				"review_gone", m.ReviewGone.Load(),
				"token_mints", m.TokenMints.Load(),
				"token_cache_hits", m.TokenCacheHits.Load(),
				"token_errors", m.TokenErrors.Load(),
				"fetch_errors", m.FetchErrors.Load(),
				"fetch_latency_ms_sum", m.FetchLatencySumMS.Load(),
				"fetch_count", m.FetchLatencyCount.Load(),
				"parse_latency_ms_sum", m.ParseLatencySumMS.Load(),
				"parse_count", m.ParseLatencyCount.Load(),
				"truncated", m.Truncated.Load(),
				"logs_archived", m.LogsArchived.Load(),
				"log_archive_errors", m.LogArchiveErrors.Load(),
				"delivered", m.Delivered.Load(),
				"deliver_retries", m.DeliverRetries.Load(),
				"deliver_errors", m.DeliverErrors.Load(),
				"rate_remaining", m.RateRemaining.Load(),
			)
		}
	}
}
