package grpc

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
	"github.com/nais/tunnel-operator/pkg/portalloc"
)

func TestGetConfigReturnsEmptyConfigWhenNoTunnels(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	server := NewForwarderServer(fake.NewClientBuilder().WithScheme(scheme).Build(), portalloc.New(10000, 10010), client.ObjectKey{})

	config, err := server.GetConfig(context.Background(), &forwarderv1.GetConfigRequest{})
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	if len(config.GetTunnels()) != 0 {
		t.Fatalf("expected no tunnel mappings, got %d", len(config.GetTunnels()))
	}
	if config.LbVip != nil {
		t.Fatalf("expected no load balancer VIP, got %q", config.GetLbVip())
	}
}

func TestGetConfigReturnsTunnelMappingsForAllocatedTunnels(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	allocator := portalloc.New(10000, 10010)
	forwarderSvcKey := client.ObjectKey{Name: "forwarder-svc", Namespace: "default"}
	server := NewForwarderServer(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&v1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: "tunnel-a", Namespace: "default"},
				Status: v1alpha1.TunnelStatus{
					ForwarderPort:  10005,
					GatewayPodName: "gateway-a",
				},
			},
			&v1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: "tunnel-b", Namespace: "default"},
			},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "gateway-a", Namespace: "default"},
				Status:     corev1.PodStatus{PodIP: "10.42.0.15"},
			},
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "forwarder-svc", Namespace: "default"},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}},
					},
				},
			},
		).Build(),
		allocator,
		forwarderSvcKey,
	)

	config, err := server.GetConfig(context.Background(), &forwarderv1.GetConfigRequest{})
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	if got := len(config.GetTunnels()); got != 1 {
		t.Fatalf("expected 1 tunnel mapping, got %d", got)
	}
	if config.GetLbVip() != "192.0.2.10" {
		t.Fatalf("expected lb VIP to be preserved, got %q", config.GetLbVip())
	}

	mapping := config.GetTunnels()[0]
	if mapping.GetTunnelName() != "tunnel-a" {
		t.Fatalf("expected tunnel name tunnel-a, got %q", mapping.GetTunnelName())
	}
	if mapping.GetTunnelNamespace() != "default" {
		t.Fatalf("expected namespace default, got %q", mapping.GetTunnelNamespace())
	}
	if mapping.GetForwarderPort() != 10005 {
		t.Fatalf("expected forwarder port 10005, got %d", mapping.GetForwarderPort())
	}
	if mapping.GetGatewayAddress() != "10.42.0.15:51820" {
		t.Fatalf("expected gateway address 10.42.0.15:51820, got %q", mapping.GetGatewayAddress())
	}

	port, ok := allocator.GetPort("default/tunnel-a")
	if !ok || port != 10005 {
		t.Fatalf("expected allocator to load existing port 10005, got %d, exists=%v", port, ok)
	}
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(client-go) error = %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(tunnel api) error = %v", err)
	}

	return scheme
}
