package forwarder

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConfigClientReconnectsAndResyncsSnapshot(t *testing.T) {
	server := &reconnectConfigServer{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	forwarderv1.RegisterForwarderConfigServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	client := NewConfigClient(testLogger())
	if err := client.Connect(context.Background(), listener.Addr().String()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var snapshots [][]*forwarderv1.TunnelMapping
	done := make(chan error, 1)
	go func() {
		done <- client.WatchUpdates(ctx, "forwarder-a", func(config *forwarderv1.ForwarderConfig) error {
			mu.Lock()
			snapshots = append(snapshots, config.GetTunnels())
			count := len(snapshots)
			mu.Unlock()
			if count == 2 {
				cancel()
			}
			return nil
		}, func(*forwarderv1.TunnelUpdate) error { return nil })
	}()

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second snapshot")
	}
	if err := <-done; err != context.Canceled && err != context.DeadlineExceeded {
		t.Fatalf("WatchUpdates: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(snapshots) != 2 {
		t.Fatalf("expected two snapshots, got %d", len(snapshots))
	}
	if got := len(snapshots[0]); got != 1 {
		t.Fatalf("initial snapshot mappings = %d, want 1", got)
	}
	if got := len(snapshots[1]); got != 0 {
		t.Fatalf("reconnect snapshot mappings = %d, want 0", got)
	}
	if server.forwarderID != "forwarder-a" {
		t.Fatalf("stream forwarder ID = %q, want forwarder-a", server.forwarderID)
	}
}

type reconnectConfigServer struct {
	forwarderv1.UnimplementedForwarderConfigServiceServer
	mu          sync.Mutex
	streamCalls int
	forwarderID string
}

func (s *reconnectConfigServer) GetConfig(
	context.Context, *forwarderv1.GetConfigRequest,
) (*forwarderv1.ForwarderConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamCalls < 2 {
		return &forwarderv1.ForwarderConfig{Tunnels: []*forwarderv1.TunnelMapping{{
			TunnelName:     new("tunnel"),
			ForwarderPort:  new(int32(51820)),
			GatewayAddress: new("10.0.0.1:51820"),
			Revision:       new(int64(1)),
		}}}, nil
	}
	return &forwarderv1.ForwarderConfig{}, nil
}

func (s *reconnectConfigServer) StreamUpdates(
	request *forwarderv1.StreamUpdatesRequest,
	stream forwarderv1.ForwarderConfigService_StreamUpdatesServer,
) error {
	s.mu.Lock()
	s.streamCalls++
	call := s.streamCalls
	s.forwarderID = request.GetForwarderId()
	s.mu.Unlock()
	if err := stream.Send(&forwarderv1.TunnelUpdate{Type: forwarderv1.UpdateType_SYNC.Enum()}); err != nil {
		return err
	}
	if call == 1 {
		return status.Error(codes.Unavailable, "simulate disconnected stream")
	}
	<-stream.Context().Done()
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
