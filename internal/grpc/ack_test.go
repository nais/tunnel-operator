package grpc

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
)

func TestForwarderServerTracksAcksFromConnectedForwarders(t *testing.T) {
	t.Parallel()

	server := NewForwarderServer(fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build(), nil, client.ObjectKey{})
	forwarderA := &forwarderStream{id: "forwarder-a", updates: make(chan *forwarderv1.TunnelUpdate, 1), overflow: make(chan struct{})}
	forwarderB := &forwarderStream{id: "forwarder-b", updates: make(chan *forwarderv1.TunnelUpdate, 1), overflow: make(chan struct{})}
	server.addStream(forwarderA)
	server.addStream(forwarderB)
	t.Cleanup(func() {
		server.removeStream(forwarderA)
		server.removeStream(forwarderB)
	})

	ack := func(id string, revision int64) {
		t.Helper()
		if _, err := server.Ack(context.Background(), &forwarderv1.AckRequest{
			ForwarderId: new(id), TunnelNamespace: new("default"), TunnelName: new("tunnel"), Revision: new(revision),
		}); err != nil {
			t.Fatalf("ack %s: %v", id, err)
		}
	}
	ack("forwarder-a", 2)
	if missing := server.MissingAcks("default", "tunnel", 2); len(missing) != 1 || missing[0] != "forwarder-b" {
		t.Fatalf("missing acks after one acknowledgement = %v, want [forwarder-b]", missing)
	}
	ack("forwarder-b", 2)
	if missing := server.MissingAcks("default", "tunnel", 2); len(missing) != 0 {
		t.Fatalf("missing acks after both acknowledgements = %v, want none", missing)
	}

	server.removeStream(forwarderB)
	if missing := server.MissingAcks("default", "tunnel", 3); len(missing) != 1 || missing[0] != "forwarder-a" {
		t.Fatalf("disconnected forwarder was still required: %v", missing)
	}
}
