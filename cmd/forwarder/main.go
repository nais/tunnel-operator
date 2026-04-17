package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nais/tunnel-operator/pkg/forwarder"
	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
)

const (
	defaultHealthAddr = ":8080"
	proxyIdleTimeout  = 5 * time.Minute
	retryInterval     = 5 * time.Second
	drainTimeout      = 30 * time.Second
	fetchTimeout      = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(context.Background(), logger); err != nil {
		logger.Error("forwarder error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if level := strings.TrimSpace(os.Getenv("LOG_LEVEL")); level != "" {
		parsed, err := parseLogLevel(level)
		if err != nil {
			return err
		}
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parsed}))
		slog.SetDefault(logger)
	}

	operatorAddr := requireEnv("OPERATOR_GRPC_ADDR")
	healthAddr := envOrDefault("HEALTH_ADDR", defaultHealthAddr)
	lbVIP := strings.TrimSpace(os.Getenv("LB_VIP"))

	proxy := forwarder.NewUDPProxy(proxyIdleTimeout)
	if lbVIP != "" {
		proxy.SetVIP(lbVIP)
	}

	configClient := forwarder.NewConfigClient(logger)
	defer func() {
		if err := configClient.Close(); err != nil {
			logger.Error("close gRPC client", "err", err)
		}
	}()

	healthServer := forwarder.NewHealthServer(healthAddr)
	go healthServer.Start(ctx)

	logger.Info("forwarder starting", "operatorAddr", operatorAddr, "healthAddr", healthAddr, "lbVIP", lbVIP)

	config, err := connectAndFetchConfig(ctx, configClient, operatorAddr, logger)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return drainForwarder(proxy, healthServer, logger)
		}
		return err
	}

	if lbVIP == "" {
		lbVIP = strings.TrimSpace(config.GetLbVip())
		if lbVIP != "" {
			proxy.SetVIP(lbVIP)
		}
	}

	if err := applyMappings(ctx, proxy, config.GetTunnels()); err != nil {
		return err
	}

	healthServer.SetReady(true)
	logger.Info("forwarder ready", "tunnels", len(config.GetTunnels()), "lbVIP", lbVIP)

	watchErrCh := make(chan error, 1)
	go func() {
		watchErrCh <- configClient.WatchUpdates(ctx, func(update *forwarderv1.TunnelUpdate) {
			handleUpdate(ctx, proxy, logger, update)
		})
	}()

	select {
	case <-ctx.Done():
		return drainForwarder(proxy, healthServer, logger)
	case err := <-watchErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("watch updates: %w", err)
		}
		return drainForwarder(proxy, healthServer, logger)
	}
}

func connectAndFetchConfig(ctx context.Context, configClient *forwarder.ConfigClient, operatorAddr string, logger *slog.Logger) (*forwarderv1.ForwarderConfig, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if err := configClient.Connect(ctx, operatorAddr); err != nil {
			logger.Error("connect to operator failed", "operatorAddr", operatorAddr, "err", err)
		} else {
			fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
			config, err := configClient.FetchConfig(fetchCtx)
			cancel()
			if err == nil {
				return config, nil
			}

			logger.Error("fetch initial config failed", "operatorAddr", operatorAddr, "err", err)
			if closeErr := configClient.Close(); closeErr != nil {
				logger.Error("close failed gRPC client", "err", closeErr)
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

func applyMappings(ctx context.Context, proxy *forwarder.UDPProxy, mappings []*forwarderv1.TunnelMapping) error {
	for _, mapping := range mappings {
		if mapping == nil {
			continue
		}

		port := int(mapping.GetForwarderPort())
		gatewayAddr := strings.TrimSpace(mapping.GetGatewayAddress())
		if port <= 0 || gatewayAddr == "" {
			continue
		}

		if err := proxy.AddMapping(ctx, port, gatewayAddr); err != nil {
			return fmt.Errorf("add mapping for port %d: %w", port, err)
		}
	}

	return nil
}

func handleUpdate(ctx context.Context, proxy *forwarder.UDPProxy, logger *slog.Logger, update *forwarderv1.TunnelUpdate) {
	if update == nil || update.GetTunnel() == nil {
		logger.Warn("ignoring invalid tunnel update")
		return
	}

	mapping := update.GetTunnel()
	port := int(mapping.GetForwarderPort())
	if port <= 0 {
		logger.Warn("ignoring tunnel update with invalid port", "port", port)
		return
	}

	switch update.GetType() {
	case forwarderv1.UpdateType_ADDED, forwarderv1.UpdateType_MODIFIED:
		gatewayAddr := strings.TrimSpace(mapping.GetGatewayAddress())
		if gatewayAddr == "" {
			logger.Warn("ignoring tunnel update with empty gateway address", "port", port, "type", update.GetType().String())
			return
		}

		if err := proxy.AddMapping(ctx, port, gatewayAddr); err != nil {
			logger.Error("failed to apply tunnel update", "port", port, "gatewayAddr", gatewayAddr, "type", update.GetType().String(), "err", err)
			return
		}

		logger.Info("applied tunnel update", "port", port, "gatewayAddr", gatewayAddr, "type", update.GetType().String())
	case forwarderv1.UpdateType_DELETED:
		proxy.RemoveMapping(port)
		logger.Info("removed tunnel mapping", "port", port)
	default:
		logger.Warn("ignoring unknown tunnel update type", "type", update.GetType().String(), "port", port)
	}
}

func drainForwarder(proxy *forwarder.UDPProxy, healthServer *forwarder.HealthServer, logger *slog.Logger) error {
	healthServer.SetReady(false)
	logger.Info("draining connections")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := proxy.Stop(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("stop UDP proxy: %w", err)
	}

	logger.Info("forwarder shutting down")
	return nil
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}

	return v
}

func requireEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		slog.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid LOG_LEVEL: %q", raw)
	}
}
