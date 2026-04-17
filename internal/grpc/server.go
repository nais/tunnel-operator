package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	gogrpc "google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
	"github.com/nais/tunnel-operator/pkg/portalloc"
)

const gatewayPort = 51820

type ForwarderServer struct {
	forwarderv1.UnimplementedForwarderConfigServiceServer
	client              client.Client
	allocator           *portalloc.PortAllocator
	forwarderServiceKey client.ObjectKey
	mu                  sync.RWMutex
	streams             []chan *forwarderv1.TunnelUpdate
}

func NewForwarderServer(client client.Client, allocator *portalloc.PortAllocator, forwarderServiceKey client.ObjectKey) *ForwarderServer {
	return &ForwarderServer{
		client:              client,
		allocator:           allocator,
		forwarderServiceKey: forwarderServiceKey,
		streams:             make([]chan *forwarderv1.TunnelUpdate, 0),
	}
}

func (s *ForwarderServer) GetConfig(ctx context.Context, _ *forwarderv1.GetConfigRequest) (*forwarderv1.ForwarderConfig, error) {
	tunnels := &v1alpha1.TunnelList{}
	if err := s.client.List(ctx, tunnels); err != nil {
		return nil, fmt.Errorf("listing tunnels: %w", err)
	}

	mappings := make([]*forwarderv1.TunnelMapping, 0, len(tunnels.Items))
	for i := range tunnels.Items {
		tunnel := &tunnels.Items[i]
		if tunnel.Status.ForwarderPort <= 0 {
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

func (s *ForwarderServer) StreamUpdates(_ *forwarderv1.StreamUpdatesRequest, stream forwarderv1.ForwarderConfigService_StreamUpdatesServer) error {
	updates := make(chan *forwarderv1.TunnelUpdate, 16)
	s.addStream(updates)
	defer s.removeStream(updates)

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case update := <-updates:
			if update == nil {
				continue
			}
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}
}

func (s *ForwarderServer) NotifyUpdate(update *forwarderv1.TunnelUpdate) {
	if update == nil {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, stream := range s.streams {
		select {
		case stream <- update:
		default:
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
	defer listener.Close()

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

func (s *ForwarderServer) addStream(stream chan *forwarderv1.TunnelUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.streams = append(s.streams, stream)
}

func (s *ForwarderServer) removeStream(stream chan *forwarderv1.TunnelUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, candidate := range s.streams {
		if candidate != stream {
			continue
		}

		s.streams = append(s.streams[:i], s.streams[i+1:]...)
		close(stream)
		return
	}
}

func (s *ForwarderServer) tunnelMapping(ctx context.Context, tunnel *v1alpha1.Tunnel) (*forwarderv1.TunnelMapping, error) {
	gatewayAddress := net.JoinHostPort(tunnel.Status.GatewayPodName, strconv.Itoa(gatewayPort))
	if tunnel.Status.GatewayPodName != "" {
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
	}, nil
}

func tunnelKey(namespace, name string) string {
	return namespace + "/" + name
}

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
