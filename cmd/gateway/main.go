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
	"syscall"
	"time"

	naisiov1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	"github.com/nais/tunnel-operator/pkg/wireguard"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var tunnelGVR = schema.GroupVersionResource{
	Group:    "nais.io",
	Version:  "v1alpha1",
	Resource: "tunnels",
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("get in-cluster config: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	logger.Info("updating Tunnel CR status")
	err = updateTunnelStatus(
		ctx, dynClient, tunnelNamespace, tunnelName,
		publicKey.String(), naisiov1alpha1.TunnelPhaseReady, "Gateway ready",
	)
	if err != nil {
		return fmt.Errorf("update tunnel status: %w", err)
	}

	logger.Info("creating WireGuard device", "listenPort", 51820)
	dev, err := wireguard.NewDevice(privateKey, peerPublicKey, "", 51820, wireguard.TunnelIPGateway)
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

	errCh := make(chan error, 1)
	activityCh := make(chan struct{}, 1)
	go serveTCPProxy(ctx, listener, targetAddr, logger, errCh, activityCh, metrics)

	peerTimeout := time.Duration(3*wireguard.PersistentKeepalive) * time.Second
	logger.Info("gateway ready, waiting for peer connection", "peerTimeout", peerTimeout.String())

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
			_ = updateTunnelStatus(
				context.Background(), dynClient, tunnelNamespace, tunnelName,
				publicKey.String(), naisiov1alpha1.TunnelPhaseFailed, err.Error(),
			)
			return err
		}
	case <-peerTimeoutCh:
		logger.Info("peer disconnected (no activity timeout)", "timeout", peerTimeout.String())
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	err = updateTunnelStatus(
		shutdownCtx, dynClient, tunnelNamespace, tunnelName,
		publicKey.String(), naisiov1alpha1.TunnelPhaseTerminated, "Gateway terminated",
	)
	if err != nil {
		logger.Error("failed to update termination status", "err", err)
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

func updateTunnelStatus(
	ctx context.Context, client dynamic.Interface,
	namespace, name, pubKey string, phase naisiov1alpha1.TunnelPhase, message string,
) error {
	patch := map[string]any{
		"status": map[string]any{
			"phase":            string(phase),
			"gatewayPublicKey": pubKey,
			"message":          message,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}

	_, err = client.Resource(tunnelGVR).Namespace(namespace).Patch(
		ctx,
		name,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
		"status",
	)
	if err != nil {
		return fmt.Errorf("patch status: %w", err)
	}

	return nil
}

func requireEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		slog.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}
