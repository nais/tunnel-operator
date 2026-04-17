package forwarder

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"
)

func TestUDPProxyAddMappingForwardsPackets(t *testing.T) {
	t.Parallel()

	echoConn := startUDPEchoServer(t)
	proxyPort := reserveUDPPort(t)
	proxy := NewUDPProxy(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		proxy.Close()
	})

	if err := proxy.AddMapping(ctx, proxyPort, echoConn.LocalAddr().String(), "test-tunnel", "default"); err != nil {
		t.Fatalf("add mapping: %v", err)
	}

	clientConn := listenUDP(t)
	payload := []byte("hello through proxy")
	response := exchangeUDP(t, clientConn, udpAddr(t, proxyPort), payload)

	if !bytes.Equal(response, payload) {
		t.Fatalf("unexpected response: got %q want %q", response, payload)
	}

	listener := mappingForPort(t, proxy, proxyPort)
	if _, ok := listener.sessions.Get(clientConn.LocalAddr().String()); !ok {
		t.Fatalf("expected session for client %s", clientConn.LocalAddr())
	}
}

func TestUDPProxyRemoveMappingStopsListening(t *testing.T) {
	t.Parallel()

	echoConn := startUDPEchoServer(t)
	proxyPort := reserveUDPPort(t)
	proxy := NewUDPProxy(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		proxy.Close()
	})

	if err := proxy.AddMapping(ctx, proxyPort, echoConn.LocalAddr().String(), "test-tunnel", "default"); err != nil {
		t.Fatalf("add mapping: %v", err)
	}

	proxy.RemoveMapping(proxyPort)

	if _, exists := proxy.mappings[proxyPort]; exists {
		t.Fatalf("expected mapping for port %d to be removed", proxyPort)
	}

	rebound, err := net.ListenUDP("udp4", udpAddr(t, proxyPort))
	if err != nil {
		t.Fatalf("expected port %d to be free after RemoveMapping: %v", proxyPort, err)
	}
	_ = rebound.Close()
}

func TestUDPProxySessionCleanupRemovesIdleSession(t *testing.T) {
	t.Parallel()

	echoConn := startUDPEchoServer(t)
	proxyPort := reserveUDPPort(t)
	proxy := NewUDPProxy(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		proxy.Close()
	})

	if err := proxy.AddMapping(ctx, proxyPort, echoConn.LocalAddr().String(), "test-tunnel", "default"); err != nil {
		t.Fatalf("add mapping: %v", err)
	}

	clientConn := listenUDP(t)
	payload := []byte("idle me out")
	_ = exchangeUDP(t, clientConn, udpAddr(t, proxyPort), payload)

	listener := mappingForPort(t, proxy, proxyPort)
	key := clientConn.LocalAddr().String()
	session, ok := listener.sessions.Get(key)
	if !ok {
		t.Fatalf("expected session for client %s", key)
	}

	before := runtime.NumGoroutine()
	setSessionLastActivity(session, time.Now().Add(-2*time.Second))
	listener.sessions.CleanupIdle(proxy.idleTimeout)

	if _, ok := listener.sessions.Get(key); ok {
		t.Fatalf("expected idle session for client %s to be removed", key)
	}

	if _, err := session.upstreamConn.Write([]byte("after cleanup")); err == nil {
		t.Fatal("expected upstream connection to be closed after cleanup")
	}

	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		return runtime.NumGoroutine() <= before
	})
}

func startUDPEchoServer(t *testing.T) *net.UDPConn {
	t.Helper()

	conn := listenUDP(t)
	done := make(chan struct{})

	go func() {
		defer close(done)

		buf := make([]byte, udpBufferSize)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}

			if _, err := conn.WriteToUDP(buf[:n], addr); err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for echo server shutdown")
		}
	})

	return conn
}

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()

	conn, err := net.ListenUDP("udp4", udpAddr(t, 0))
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	return conn
}

func reserveUDPPort(t *testing.T) int {
	t.Helper()

	conn, err := net.ListenUDP("udp4", udpAddr(t, 0))
	if err != nil {
		t.Fatalf("reserve udp port: %v", err)
	}
	defer func() { _ = conn.Close() }()

	return conn.LocalAddr().(*net.UDPAddr).Port
}

func exchangeUDP(t *testing.T, clientConn *net.UDPConn, target *net.UDPAddr, payload []byte) []byte {
	t.Helper()

	if err := clientConn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	defer func() {
		_ = clientConn.SetDeadline(time.Time{})
	}()

	if _, err := clientConn.WriteToUDP(payload, target); err != nil {
		t.Fatalf("write udp: %v", err)
	}

	buf := make([]byte, len(payload)+64)
	n, _, err := clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read udp response: %v", err)
	}

	return append([]byte(nil), buf[:n]...)
}

func mappingForPort(t *testing.T, proxy *UDPProxy, port int) *portListener {
	t.Helper()

	proxy.mu.RLock()
	defer proxy.mu.RUnlock()

	listener, ok := proxy.mappings[port]
	if !ok {
		t.Fatalf("expected mapping for port %d", port)
	}

	return listener
}

func udpAddr(t *testing.T, port int) *net.UDPAddr {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}

	return addr
}

func setSessionLastActivity(session *Session, ts time.Time) {
	session.mu.Lock()
	defer session.mu.Unlock()

	session.lastActivity = ts
}

func eventually(t *testing.T, timeout, interval time.Duration, fn func() bool) {
	t.Helper()

	if fn() {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			if fn() {
				return
			}
		case <-timer.C:
			t.Fatal("condition not satisfied before timeout")
		}
	}
}
