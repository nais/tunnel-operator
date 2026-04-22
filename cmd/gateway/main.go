package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nais/tunnel-operator/pkg/wireguard"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func main() {
	logLevel := parseLogLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	if err := run(context.Background(), logger); err != nil {
		logger.Error("gateway error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	peerPublicKeyStr := requireEnv("TUNNEL_PEER_PUBLIC_KEY")
	targetHost := requireEnv("TUNNEL_TARGET_HOST")
	targetPortStr := requireEnv("TUNNEL_TARGET_PORT")
	tunnelName := requireEnv("TUNNEL_NAME")
	tunnelNamespace := requireEnv("TUNNEL_NAMESPACE")

	metrics := newGatewayMetrics(tunnelName, tunnelNamespace, targetHost, targetPortStr)

	var publicKeyValue atomic.Pointer[string]

	statusMux := http.NewServeMux()
	statusMux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		pk := publicKeyValue.Load()
		if pk == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"publicKey": *pk})
	})
	go func() {
		statusServer := &http.Server{
			Addr:              ":8080",
			Handler:           statusMux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		if err := statusServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("status server error", "err", err)
		}
	}()

	go func() {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		metricsServer := &http.Server{
			Addr:              ":9091",
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server error", "err", err)
		}
	}()

	targetPort, err := strconv.Atoi(targetPortStr)
	if err != nil {
		return fmt.Errorf("invalid TUNNEL_TARGET_PORT: %w", err)
	}

	peerPublicKey, err := wgtypes.ParseKey(peerPublicKeyStr)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}

	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return fmt.Errorf("generate private key: %w", err)
	}
	publicKey := privateKey.PublicKey()
	logger.Info("generated WireGuard keypair", "publicKey", publicKey.String())

	logger.Info("creating WireGuard device", "listenPort", 51820, "gatewayPublicKey", publicKey.String(), "peerPublicKey", peerPublicKey.String())
	dev, err := wireguard.NewDevice(privateKey, peerPublicKey, "", 51820, wireguard.TunnelIPGateway, logger)
	if err != nil {
		return fmt.Errorf("create WireGuard device: %w", err)
	}
	defer dev.Close()

	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))
	listenAddr := &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: targetPort}
	logger.Info("starting TCP proxy", "listen", listenAddr.String(), "target", targetAddr)

	listener, err := dev.Net().ListenTCP(listenAddr)
	if err != nil {
		return fmt.Errorf("listen TCP on netstack: %w", err)
	}
	defer func() { _ = listener.Close() }()

	pkStr := publicKey.String()
	publicKeyValue.Store(&pkStr)
	logger.Info("gateway ready", "publicKey", pkStr)

	errCh := make(chan error, 1)
	activityCh := make(chan struct{}, 1)
	go serveTCPProxy(ctx, listener, targetAddr, logger, errCh, activityCh, metrics)

	peerTimeout := 1 * time.Hour
	logger.Info("waiting for peer connection", "peerTimeout", peerTimeout.String())

	peerTimeoutCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-activityCh:
		}
		timer := time.NewTimer(peerTimeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activityCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(peerTimeout)
			case <-timer.C:
				close(peerTimeoutCh)
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("gateway shutting down")
	case err := <-errCh:
		if err != nil {
			return err
		}
	case <-peerTimeoutCh:
		logger.Info("peer disconnected (no activity timeout)", "timeout", peerTimeout.String())
	}

	return nil
}

func serveTCPProxy(
	ctx context.Context, listener net.Listener, targetAddr string,
	logger *slog.Logger, errCh chan<- error, activityCh chan<- struct{}, m *gatewayMetrics,
) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				logger.Error("accept TCP connection", "err", err)
				select {
				case errCh <- fmt.Errorf("accept TCP connection: %w", err):
				default:
				}
				return
			}
		}

		select {
		case activityCh <- struct{}{}:
		default:
		}
		go handleConn(conn, targetAddr, logger, m)
	}
}

func handleConn(src net.Conn, targetAddr string, logger *slog.Logger, m *gatewayMetrics) {
	defer func() { _ = src.Close() }()

	m.tcpConnectionsTotal.Inc()
	m.tcpConnectionsActive.Inc()
	defer m.tcpConnectionsActive.Dec()

	dst, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		logger.Error("dial target", "addr", targetAddr, "err", err)
		return
	}
	defer func() { _ = dst.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		n, _ := copyAndClose(dst, src)
		m.tcpReceivedBytes.Add(float64(n))
		done <- struct{}{}
	}()
	go func() {
		n, _ := copyAndClose(src, dst)
		m.tcpSentBytes.Add(float64(n))
		done <- struct{}{}
	}()
	<-done
	<-done
}

func copyAndClose(dst, src net.Conn) (int64, error) {
	n, err := io.Copy(dst, src)
	if tc, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = tc.CloseWrite()
	}
	return n, err
}

func requireEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		slog.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
