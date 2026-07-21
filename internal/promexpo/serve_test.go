package promexpo

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeEmptyAddrIsNoop(t *testing.T) {
	if err := Serve(context.Background(), "", nil, nil); err != nil {
		t.Fatalf("empty addr should be a no-op, got %v", err)
	}
}

func TestServeLive(t *testing.T) {
	// Grab a free port, then hand its address to Serve.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, addr, nil, func() []Sample {
			return []Sample{{Name: "shuck_test_total", Help: "t", Type: Counter, Value: 11}}
		})
	}()

	base := "http://" + addr
	body := getWithRetry(t, base+"/metrics")
	if !strings.Contains(body, "shuck_test_total 11\n") {
		t.Fatalf("metrics body = %q", body)
	}
	if hb := getWithRetry(t, base+"/healthz"); !strings.Contains(hb, "ok") {
		t.Fatalf("healthz body = %q", hb)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not shut down after ctx cancel")
	}
}

func getWithRetry(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(url) //nolint:noctx // test helper
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return string(b)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s never succeeded: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
