package forwarder

import (
	"context"
	"fmt"
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

func NewConfigClient(logger *slog.Logger) *ConfigClient { return &ConfigClient{logger: logger} }

func (c *ConfigClient) Connect(_ context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	c.conn = conn
	c.client = forwarderv1.NewForwarderConfigServiceClient(conn)
	return nil
}

func (c *ConfigClient) FetchConfig(ctx context.Context) (*forwarderv1.ForwarderConfig, error) {
	return c.client.GetConfig(ctx, &forwarderv1.GetConfigRequest{})
}

func (c *ConfigClient) Ack(ctx context.Context, forwarderID string, mapping *forwarderv1.TunnelMapping) error {
	if mapping == nil {
		return fmt.Errorf("nil tunnel mapping")
	}
	_, err := c.client.Ack(ctx, &forwarderv1.AckRequest{
		ForwarderId:     &forwarderID,
		TunnelName:      mapping.TunnelName,
		TunnelNamespace: mapping.TunnelNamespace,
		Revision:        mapping.Revision,
	})
	return err
}

// WatchUpdates establishes a registered update stream, waits for its SYNC
// barrier, and then fetches a snapshot. Updates queued before the snapshot are
// replayed afterwards; mapping revisions make older queued updates no-ops.
// This sequence runs again for every reconnect.
func (c *ConfigClient) WatchUpdates(
	ctx context.Context,
	forwarderID string,
	onSnapshot func(*forwarderv1.ForwarderConfig) error,
	onUpdate func(*forwarderv1.TunnelUpdate) error,
) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stream, err := c.client.StreamUpdates(ctx, &forwarderv1.StreamUpdatesRequest{ForwarderId: &forwarderID})
		if err == nil {
			update, recvErr := stream.Recv()
			if recvErr != nil {
				err = recvErr
			} else if update.GetType() != forwarderv1.UpdateType_SYNC {
				err = fmt.Errorf("expected stream sync marker, got %s", update.GetType())
			}
		}
		if err == nil {
			config, fetchErr := c.FetchConfig(ctx)
			if fetchErr != nil {
				err = fmt.Errorf("fetch config after stream sync: %w", fetchErr)
			} else if callbackErr := onSnapshot(config); callbackErr != nil {
				err = fmt.Errorf("apply config snapshot: %w", callbackErr)
			}
		}
		if err == nil {
			backoff = time.Second
			for {
				update, recvErr := stream.Recv()
				if recvErr != nil {
					err = recvErr
					break
				}
				if update.GetType() == forwarderv1.UpdateType_SYNC {
					continue
				}
				if callbackErr := onUpdate(update); callbackErr != nil {
					c.logger.Error("apply tunnel update failed", "err", callbackErr)
				}
			}
		}

		c.logger.Info("stream error, reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *ConfigClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
