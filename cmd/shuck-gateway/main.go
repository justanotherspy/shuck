// Command shuck-gateway is the event-delivery service of shuck's opt-in
// self-hosted mode (JUS-88, JUS-92): it authenticates per-user bearer
// tokens, owns PR subscriptions and the per-subscriber event buffer in
// DynamoDB, and delivers worker events with write-then-push semantics. It
// runs two ways:
//
//   - Server mode (default): a long-lived process terminating the shim
//     WebSockets itself — the JUS-93 kubernetes deployment.
//   - Lambda mode (auto-detected): API Gateway's WebSocket API terminates
//     the connections and this binary serves its routes per invocation,
//     role-dispatched by SHUCK_WS_ROLE — the JUS-92 serverless deployment.
//
// Configuration is environment-only (deploy tooling injects secrets;
// JUS-92/93 own that wiring):
//
//	SHUCK_TOKEN_TABLE               DynamoDB token table (required)
//	SHUCK_SUBSCRIPTION_TABLE        DynamoDB subscription table (required)
//	SHUCK_BUFFER_TABLE              DynamoDB event buffer table (required)
//	SHUCK_DELIVER_SECRET            shared secret for /internal/deliver
//	                                (required; sweep role excepted)
//	SHUCK_DELIVER_SECRET_SECONDARY  second accepted secret during rotation
//	SHUCK_ADDR                      HTTP listen address (default :8080)
//	SHUCK_HEARTBEAT                 WS ping interval (default 30s; server mode)
//	SHUCK_GRACE_WINDOW              disconnected-subscriber retention (default 24h)
//	SHUCK_SWEEP_INTERVAL            grace-window sweep cadence (default 15m;
//	                                server mode — Lambda mode schedules the
//	                                sweep role externally)
//	SHUCK_BUFFER_TTL                buffered event retention (default 72h)
//
// Lambda mode only:
//
//	SHUCK_WS_ROLE      ws      — the WebSocket API's $connect/$default/
//	                             $disconnect routes (one function, dispatched
//	                             on the event type)
//	                   deliver — POST /internal/deliver behind a function URL
//	                   sweep   — one grace-window sweep pass per invocation
//	                             (EventBridge schedule)
//	SHUCK_WS_ENDPOINT  the WebSocket API's @connections callback endpoint,
//	                   https://{api-id}.execute-api.{region}.amazonaws.com/{stage}
//	                   (required for the ws and deliver roles)
//	SHUCK_REGISTRY_TTL registry row retention (default 3h; must outlive API
//	                   Gateway's 2h connection cap)
//
// Server-mode endpoints: GET /ws (shim WebSocket), POST /internal/deliver
// (workers, shared-secret header), GET /healthz (liveness), GET /readyz
// (readiness — 503 once draining). SIGTERM drains: readiness flips, every
// socket closes with the going-away code so shims reconnect, then the
// process exits.
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

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/justanotherspy/shuck/internal/gateway"
	"github.com/justanotherspy/shuck/internal/gateway/awsx"
	"github.com/justanotherspy/shuck/internal/gateway/serverless"
	"github.com/justanotherspy/shuck/internal/lambdahttp"
)

// drainTimeout bounds how long shutdown waits for connections to close.
const drainTimeout = 10 * time.Second

// version is stamped at build time via -X main.version (Makefile /
// Dockerfile.backend); untagged builds report "dev".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	log.Info("shuck-gateway starting", "version", version)
	if err := run(context.Background(), log); err != nil {
		log.Error("shuck-gateway failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		return runLambda(ctx, log)
	}
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
	tokens := awsx.NewDynamoTokenStore(ddb, tokenTable)
	hub := &gateway.Hub{
		Tokens:    tokens,
		Toucher:   tokens,
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

// runLambda serves one SHUCK_WS_ROLE of the serverless (API Gateway
// WebSocket) deployment: the ws routes, the deliver endpoint, or a
// scheduled sweep pass.
func runLambda(ctx context.Context, log *slog.Logger) error {
	role := os.Getenv("SHUCK_WS_ROLE")
	tokenTable := os.Getenv("SHUCK_TOKEN_TABLE")
	subTable := os.Getenv("SHUCK_SUBSCRIPTION_TABLE")
	bufferTable := os.Getenv("SHUCK_BUFFER_TABLE")
	if tokenTable == "" || subTable == "" || bufferTable == "" {
		return fmt.Errorf("SHUCK_TOKEN_TABLE, SHUCK_SUBSCRIPTION_TABLE, and SHUCK_BUFFER_TABLE are required")
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
	tokens := awsx.NewDynamoTokenStore(ddb, tokenTable)
	subs := awsx.NewDynamoSubscriptionStore(ddb, subTable)
	buffer := awsx.NewDynamoEventBuffer(ddb, bufferTable, bufferTTL)
	presence := awsx.NewDynamoPresenceStore(ddb, bufferTable)

	switch role {
	case "sweep":
		grace, err := durationEnv("SHUCK_GRACE_WINDOW", gateway.DefaultGraceWindow)
		if err != nil {
			return err
		}
		// The registry is empty in a per-invocation process; liveness comes
		// from ping-refreshed presence rows, which connected shims renew
		// every few minutes — far inside any sane grace window.
		sweeper := &gateway.Sweeper{
			Subs:        subs,
			Buffer:      buffer,
			Presence:    presence,
			Registry:    gateway.NewMemRegistry(),
			GraceWindow: grace,
			Log:         log,
			Metrics:     metrics,
		}
		log.Info("starting in Lambda mode", "role", role)
		lambda.StartWithOptions(awsx.SweepLambdaHandler(sweeper.Sweep), lambda.WithContext(ctx))
		return nil
	case "ws", "deliver":
		endpoint := os.Getenv("SHUCK_WS_ENDPOINT")
		if endpoint == "" {
			return fmt.Errorf("SHUCK_WS_ENDPOINT is required for the %s role", role)
		}
		registryTTL, err := durationEnv("SHUCK_REGISTRY_TTL", awsx.DefaultRegistryTTL)
		if err != nil {
			return err
		}
		mgmt := apigatewaymanagementapi.NewFromConfig(awsCfg, func(o *apigatewaymanagementapi.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		})
		g := &serverless.Gateway{
			Tokens:   tokens,
			Toucher:  tokens,
			Subs:     subs,
			Buffer:   buffer,
			Presence: presence,
			Registry: awsx.NewDynamoRegistryStore(ddb, bufferTable, registryTTL),
			Conns:    awsx.NewAPIGWConnAPI(mgmt),
			Log:      log,
			Metrics:  metrics,
		}
		log.Info("starting in Lambda mode", "role", role)
		if role == "ws" {
			lambda.StartWithOptions(awsx.WSLambdaHandler(g, log), lambda.WithContext(ctx))
			return nil
		}
		secret := os.Getenv("SHUCK_DELIVER_SECRET")
		if secret == "" {
			return fmt.Errorf("SHUCK_DELIVER_SECRET is required for the deliver role")
		}
		secrets := [][]byte{[]byte(secret)}
		if secondary := os.Getenv("SHUCK_DELIVER_SECRET_SECONDARY"); secondary != "" {
			secrets = append(secrets, []byte(secondary))
		}
		mux := http.NewServeMux()
		mux.Handle("POST /internal/deliver", &gateway.DeliverHandler{
			Secrets: secrets,
			Hub:     g,
			Log:     log,
			Metrics: metrics,
		})
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, "ok")
		})
		lambda.StartWithOptions(lambdahttp.FunctionURLHandler(mux), lambda.WithContext(ctx))
		return nil
	default:
		return fmt.Errorf("unknown SHUCK_WS_ROLE %q (want ws, deliver, or sweep)", role)
	}
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
