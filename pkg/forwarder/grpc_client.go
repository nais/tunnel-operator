package forwarder

import (
	"context"
	"log/slog"
	"time"

	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ConfigClient fetches tunnel configuration from the operator via gRPC.
type ConfigClient struct {
	conn   *grpc.ClientConn
	client forwarderv1.ForwarderConfigServiceClient
	logger *slog.Logger
}

func NewConfigClient(logger *slog.Logger) *ConfigClient {
	return &ConfigClient{logger: logger}
}

// Connect establishes a gRPC connection to the operator at addr (e.g. "operator:9090").
func (c *ConfigClient) Connect(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	c.conn = conn
	c.client = forwarderv1.NewForwarderConfigServiceClient(conn)
	return nil
}

// FetchConfig calls GetConfig RPC and returns the full ForwarderConfig.
func (c *ConfigClient) FetchConfig(ctx context.Context) (*forwarderv1.ForwarderConfig, error) {
	return c.client.GetConfig(ctx, &forwarderv1.GetConfigRequest{})
}

// WatchUpdates calls StreamUpdates and invokes callback for each TunnelUpdate.
// Reconnects with exponential backoff on stream error. Blocks until ctx is cancelled.
func (c *ConfigClient) WatchUpdates(ctx context.Context, callback func(*forwarderv1.TunnelUpdate)) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stream, err := c.client.StreamUpdates(ctx, &forwarderv1.StreamUpdatesRequest{})
		if err != nil {
			c.logger.Info("stream error, reconnecting", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for {
			update, err := stream.Recv()
			if err != nil {
				c.logger.Info("stream recv error, reconnecting", "err", err)
				break
			}
			callback(update)
		}
	}
}

// Close closes the underlying gRPC connection.
func (c *ConfigClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
