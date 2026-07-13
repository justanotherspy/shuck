package promexpo

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// EnvAddr names the environment variable every resident backend binary reads
// to enable the metrics listener. Empty (the default) leaves it off, so the
// endpoint is strictly opt-in and portable mode is entirely unaffected.
const EnvAddr = "SHUCK_METRICS_ADDR"

// Serve runs a dedicated metrics listener on addr until ctx is done, serving
// GET /metrics (the exposition of collect()) and GET /healthz. It is a
// separate listener from the application port so scrapers target a dedicated
// port and /metrics is never routed by the app's public ingress.
//
// A blank addr is a no-op (returns nil immediately), so callers can wire it
// unconditionally: the endpoint only exists when the operator sets the
// address. Serve blocks; run it in a goroutine.
func Serve(ctx context.Context, addr string, log *slog.Logger, collect func() []Sample) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler(collect))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if log != nil {
		log.Info("metrics listener enabled", "addr", addr)
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
