// Command shuck-gateway is the persistent event-delivery service of shuck's
// opt-in self-hosted mode (JUS-88): it terminates channel-shim WebSockets,
// authenticates per-user bearer tokens, owns PR subscriptions and the
// per-subscriber event buffer in DynamoDB, and delivers worker events with
// write-then-push semantics. It is a long-lived server (never a Lambda —
// WebSockets need a resident process).
//
// Configuration is environment-only (deploy tooling injects secrets;
// JUS-92/93 own that wiring):
//
//	SHUCK_TOKEN_TABLE               DynamoDB token table (required)
//	SHUCK_SUBSCRIPTION_TABLE        DynamoDB subscription table (required)
//	SHUCK_BUFFER_TABLE              DynamoDB event buffer table (required)
//	SHUCK_DELIVER_SECRET            shared secret for /internal/deliver (required)
//	SHUCK_DELIVER_SECRET_SECONDARY  second accepted secret during rotation
//	SHUCK_ADDR                      HTTP listen address (default :8080)
//	SHUCK_HEARTBEAT                 WS ping interval (default 30s)
//	SHUCK_GRACE_WINDOW              disconnected-subscriber retention (default 24h)
//	SHUCK_SWEEP_INTERVAL            grace-window sweep cadence (default 15m)
//	SHUCK_BUFFER_TTL                buffered event retention (default 72h)
//
// Endpoints: GET /ws (shim WebSocket), POST /internal/deliver (workers,
// shared-secret header), GET /healthz (liveness), GET /readyz (readiness —
// 503 once draining). SIGTERM drains: readiness flips, every socket closes
// with the going-away code so shims reconnect, then the process exits.
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
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/gateway/awsx"
)

// drainTimeout bounds how long shutdown waits for connections to close.
const drainTimeout = 10 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), log); err != nil {
		log.Error("shuck-gateway failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	tokenTable := os.Getenv("SHUCK_TOKEN_TABLE")
	subTable := os.Getenv("SHUCK_SUBSCRIPTION_TABLE")
	bufferTable := os.Getenv("SHUCK_BUFFER_TABLE")
	secret := os.Getenv("SHUCK_DELIVER_SECRET")
	if tokenTable == "" || subTable == "" || bufferTable == "" || secret == "" {
		return fmt.Errorf("SHUCK_TOKEN_TABLE, SHUCK_SUBSCRIPTION_TABLE, SHUCK_BUFFER_TABLE, and SHUCK_DELIVER_SECRET are required")
	}
	secrets := [][]byte{[]byte(secret)}
	if secondary := os.Getenv("SHUCK_DELIVER_SECRET_SECONDARY"); secondary != "" {
		secrets = append(secrets, []byte(secondary))
	}
	heartbeat, err := durationEnv("SHUCK_HEARTBEAT", gateway.DefaultHeartbeat)
	if err != nil {
		return err
	}
	grace, err := durationEnv("SHUCK_GRACE_WINDOW", gateway.DefaultGraceWindow)
	if err != nil {
		return err
	}
	sweepInterval, err := durationEnv("SHUCK_SWEEP_INTERVAL", gateway.DefaultSweepInterval)
	if err != nil {
		return err
	}
	bufferTTL, err := durationEnv("SHUCK_BUFFER_TTL", 72*time.Hour)
	if err != nil {
		return err
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	ddb := dynamodb.NewFromConfig(awsCfg)
	metrics := &gateway.Metrics{}
	hub := &gateway.Hub{
		Tokens:    awsx.NewDynamoTokenStore(ddb, tokenTable),
		Subs:      awsx.NewDynamoSubscriptionStore(ddb, subTable),
		Buffer:    awsx.NewDynamoEventBuffer(ddb, bufferTable, bufferTTL),
		Presence:  awsx.NewDynamoPresenceStore(ddb, bufferTable),
		Log:       log,
		Metrics:   metrics,
		Heartbeat: heartbeat,
	}
	sweeper := &gateway.Sweeper{
		Subs:        hub.Subs,
		Buffer:      hub.Buffer,
		Presence:    hub.Presence,
		Registry:    hub.Reg(),
		GraceWindow: grace,
		Interval:    sweepInterval,
		Log:         log,
		Metrics:     metrics,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.ServeWS)
	mux.Handle("POST /internal/deliver", &gateway.DeliverHandler{
		Secrets: secrets,
		Hub:     hub,
		Log:     log,
		Metrics: metrics,
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if hub.Draining() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	addr := os.Getenv("SHUCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go sweeper.Run(runCtx)
	go logMetrics(runCtx, log, metrics, hub)

	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	log.Info("shuck-gateway listening", "addr", addr)

	select {
	case err := <-errCh:
		return err
	case <-runCtx.Done():
	}

	// Drain: readiness already flips via hub.Draining once Drain starts;
	// close every socket with going-away so shims reconnect to the next
	// replica, then stop accepting HTTP.
	log.Info("draining", "connections", hub.Reg().Len())
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	hub.Drain(drainCtx)
	if err := server.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("drained")
	return nil
}

// durationEnv parses an optional duration variable.
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

// logMetrics snapshots the counters periodically so a plain log pipeline
// gets visibility without a metrics backend.
func logMetrics(ctx context.Context, log *slog.Logger, m *gateway.Metrics, hub *gateway.Hub) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Info("gateway metrics",
				"connections_live", hub.Reg().Len(),
				"connections_total", m.ConnectionsTotal.Load(),
				"connections_replaced", m.ConnectionsReplaced.Load(),
				"auth_rejected", m.AuthRejected.Load(),
				"heartbeat_failures", m.HeartbeatFailures.Load(),
				"events_buffered", m.EventsBuffered.Load(),
				"events_pushed", m.EventsPushed.Load(),
				"events_acked", m.EventsAcked.Load(),
				"events_suppressed", m.EventsSuppressed.Load(),
				"events_deduped", m.EventsDeduped.Load(),
				"buffer_depth", m.BufferDepth.Load(),
				"replay_sessions", m.ReplaySessions.Load(),
				"replay_events", m.ReplayEvents.Load(),
				"deliver_requests", m.DeliverRequests.Load(),
				"deliver_rejected", m.DeliverRejected.Load(),
				"deliver_latency_ms_sum", m.DeliverLatencySumMS.Load(),
				"deliver_latency_count", m.DeliverLatencyCount.Load(),
				"sweep_removed", m.SweepRemoved.Load(),
			)
		}
	}
}
