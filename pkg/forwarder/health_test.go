package forwarder

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHealthServerHealthz(t *testing.T) {
	t.Parallel()

	baseURL, _, cancel := startHealthServer(t)
	defer cancel()

	resp := httpGet(t, baseURL+"/healthz")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHealthServerReadyz(t *testing.T) {
	t.Parallel()

	baseURL, server, cancel := startHealthServer(t)
	defer cancel()

	resp := httpGet(t, baseURL+"/readyz")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected initial status: got %d want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	server.SetReady(true)

	resp = httpGet(t, baseURL+"/readyz")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected ready status: got %d want %d", resp.StatusCode, http.StatusOK)
	}
}

func startHealthServer(t *testing.T) (string, *HealthServer, context.CancelFunc) {
	t.Helper()

	addr := reserveTCPAddr(t)
	server := NewHealthServer(addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		server.Start(ctx)
	}()

	waitForHTTPServer(t, "http://"+addr+"/healthz")

	return "http://" + addr, server, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for health server shutdown")
		}
	}
}

func reserveTCPAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve tcp addr: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}

	return addr
}

func waitForHTTPServer(t *testing.T, url string) {
	t.Helper()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			resp, err := client.Get(url)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return
			}
		case <-timer.C:
			t.Fatalf("health server did not start listening: %s", url)
		}
	}
}

func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}

	return resp
}
