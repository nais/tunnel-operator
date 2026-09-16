package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
	"github.com/nais/tunnel-operator/pkg/portalloc"
)

const gatewayPort = 51820

type forwarderStream struct {
	id       string
	updates  chan *forwarderv1.TunnelUpdate
	overflow chan struct{}
	once     sync.Once
}

// ForwarderServer provides an ordered live update stream and authoritative
// snapshots. A stream is explicitly synchronized before a client fetches a
// snapshot, so updates cannot be lost in the gap between the two operations.
type ForwarderServer struct {
	forwarderv1.UnimplementedForwarderConfigServiceServer
	client              client.Client
	allocator           *portalloc.PortAllocator
	forwarderServiceKey client.ObjectKey
	mu                  sync.RWMutex
	streams             map[*forwarderStream]struct{}
	acks                map[string]map[string]int64
	legacyForwarderSeq  atomic.Uint64
}

func NewForwarderServer(client client.Client, allocator *portalloc.PortAllocator, forwarderServiceKey client.ObjectKey) *ForwarderServer {
	return &ForwarderServer{
		client:              client,
		allocator:           allocator,
		forwarderServiceKey: forwarderServiceKey,
		streams:             make(map[*forwarderStream]struct{}),
		acks:                make(map[string]map[string]int64),
	}
}

func (s *ForwarderServer) GetConfig(ctx context.Context, _ *forwarderv1.GetConfigRequest) (*forwarderv1.ForwarderConfig, error) {
	slog.Info("GetConfig called")
	tunnels := &v1alpha1.TunnelList{}
	if err := s.client.List(ctx, tunnels); err != nil {
		return nil, fmt.Errorf("listing tunnels: %w", err)
	}

	mappings := make([]*forwarderv1.TunnelMapping, 0, len(tunnels.Items))
	for i := range tunnels.Items {
		tunnel := &tunnels.Items[i]
		if tunnel.DeletionTimestamp != nil || tunnel.Status.Phase == v1alpha1.TunnelPhaseTerminated ||
			tunnel.Status.Phase == v1alpha1.TunnelPhaseFailed || tunnel.Status.ForwarderPort <= 0 ||
			tunnel.Status.GatewayPodIP == "" || tunnel.Status.GatewayPublicKey == "" {
			continue
		}
		if s.allocator != nil {
			s.allocator.LoadExisting(tunnelKey(tunnel.Namespace, tunnel.Name), tunnel.Status.ForwarderPort)
		}
		mapping, err := s.tunnelMapping(ctx, tunnel)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}

	config := &forwarderv1.ForwarderConfig{Tunnels: mappings}
	if vip := s.resolveVIP(ctx); vip != "" {
		config.LbVip = &vip
	}
	return config, nil
}

func (s *ForwarderServer) StreamUpdates(request *forwarderv1.StreamUpdatesRequest, stream forwarderv1.ForwarderConfigService_StreamUpdatesServer) error {
	forwarder := &forwarderStream{
		id: s.streamID(request.GetForwarderId()), updates: make(chan *forwarderv1.TunnelUpdate, 64), overflow: make(chan struct{}),
	}
	s.addStream(forwarder)
	defer s.removeStream(forwarder)

	// Sending SYNC after registering the stream establishes the snapshot barrier.
	if err := stream.Send(&forwarderv1.TunnelUpdate{Type: forwarderv1.UpdateType_SYNC.Enum()}); err != nil {
		return err
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-forwarder.overflow:
			return status.Error(codes.ResourceExhausted, "forwarder update stream overflowed; reconnect to resync")
		case update := <-forwarder.updates:
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}
}

// Ack records a successfully applied mapping revision for a connected forwarder.
func (s *ForwarderServer) Ack(_ context.Context, request *forwarderv1.AckRequest) (*forwarderv1.AckResponse, error) {
	if request.GetForwarderId() == "" || request.GetTunnelName() == "" || request.GetTunnelNamespace() == "" {
		return nil, status.Error(codes.InvalidArgument, "forwarder_id, tunnel_name, and tunnel_namespace are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connectedLocked(request.GetForwarderId()) {
		return nil, status.Error(codes.FailedPrecondition, "forwarder is not connected")
	}
	if s.acks[request.GetForwarderId()] == nil {
		s.acks[request.GetForwarderId()] = make(map[string]int64)
	}
	key := tunnelKey(request.GetTunnelNamespace(), request.GetTunnelName())
	if request.GetRevision() > s.acks[request.GetForwarderId()][key] {
		s.acks[request.GetForwarderId()][key] = request.GetRevision()
	}
	return &forwarderv1.AckResponse{}, nil
}

// MissingAcks returns whether every forwarder connected at the time of the
// call has acknowledged at least revision for the tunnel.
func (s *ForwarderServer) MissingAcks(namespace, name string, revision int64) []string {
	key := tunnelKey(namespace, name)
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make(map[string]struct{})
	for stream := range s.streams {
		ids[stream.id] = struct{}{}
	}
	missing := make([]string, 0)
	for id := range ids {
		if s.acks[id][key] < revision {
			missing = append(missing, id)
		}
	}
	return missing
}

func (s *ForwarderServer) NotifyUpdate(update *forwarderv1.TunnelUpdate) {
	if update == nil {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for forwarder := range s.streams {
		select {
		case forwarder.updates <- update:
		default:
			// Never silently lose an update. The client reconnects and performs
			// an authoritative snapshot reconciliation after this stream ends.
			forwarder.once.Do(func() { close(forwarder.overflow) })
		}
	}
}

func (s *ForwarderServer) Start(ctx context.Context, addr string) error {
	if addr == "" {
		addr = ":9090"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer func() { _ = listener.Close() }()

	server := gogrpc.NewServer()
	forwarderv1.RegisterForwarderConfigServiceServer(server, s)
	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
		return fmt.Errorf("serving grpc: %w", err)
	}
	return nil
}

func (s *ForwarderServer) streamID(id string) string {
	if id != "" {
		return id
	}

	// Older forwarders cannot acknowledge revisions. Keep their data path alive
	// during a rolling upgrade, but let this stream block Ready until it has been
	// replaced by an acknowledgement-capable forwarder.
	id = fmt.Sprintf("legacy-%d", s.legacyForwarderSeq.Add(1))
	slog.Warn("legacy forwarder connected without an identity", "forwarderID", id)
	return id
}

func (s *ForwarderServer) addStream(stream *forwarderStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[stream] = struct{}{}
}

func (s *ForwarderServer) removeStream(stream *forwarderStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, stream)
	if !s.connectedLocked(stream.id) {
		delete(s.acks, stream.id)
	}
}

func (s *ForwarderServer) connectedLocked(id string) bool {
	for stream := range s.streams {
		if stream.id == id {
			return true
		}
	}
	return false
}

func (s *ForwarderServer) tunnelMapping(ctx context.Context, tunnel *v1alpha1.Tunnel) (*forwarderv1.TunnelMapping, error) {
	gatewayAddress := net.JoinHostPort(tunnel.Status.GatewayPodIP, strconv.Itoa(gatewayPort))
	if tunnel.Status.GatewayPodIP == "" {
		gatewayAddress = net.JoinHostPort(tunnel.Status.GatewayPodName, strconv.Itoa(gatewayPort))
	}
	if tunnel.Status.GatewayPodName != "" && tunnel.Status.GatewayPodIP == "" {
		pod := &corev1.Pod{}
		err := s.client.Get(ctx, client.ObjectKey{Namespace: tunnel.Namespace, Name: tunnel.Status.GatewayPodName}, pod)
		switch {
		case err == nil && pod.Status.PodIP != "":
			gatewayAddress = net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(gatewayPort))
		case err == nil:
		case apierrors.IsNotFound(err):
		default:
			return nil, fmt.Errorf("getting gateway pod %s/%s: %w", tunnel.Namespace, tunnel.Status.GatewayPodName, err)
		}
	}

	return &forwarderv1.TunnelMapping{
		TunnelName:      &tunnel.Name,
		TunnelNamespace: &tunnel.Namespace,
		ForwarderPort:   &tunnel.Status.ForwarderPort,
		GatewayAddress:  &gatewayAddress,
		Revision:        &tunnel.Status.MappingRevision,
		GatewayPodUid:   &tunnel.Status.GatewayPodUID,
		GatewayPodIp:    &tunnel.Status.GatewayPodIP,
	}, nil
}

func tunnelKey(namespace, name string) string { return namespace + "/" + name }

func (s *ForwarderServer) resolveVIP(ctx context.Context) string {
	if s.forwarderServiceKey.Name == "" {
		return ""
	}
	svc := &corev1.Service{}
	if err := s.client.Get(ctx, s.forwarderServiceKey, svc); err != nil {
		return ""
	}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
	}
	return ""
}
