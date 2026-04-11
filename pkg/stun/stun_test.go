package stun

import (
	"net"
	"regexp"
	"testing"
)

func TestDiscoverEndpoint(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	defer conn.Close()

	endpoint, err := DiscoverEndpoint(conn, DefaultSTUNServers)
	if err != nil {
		t.Fatalf("discover endpoint: %v", err)
	}

	pattern := regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+:\d+$`)
	if !pattern.MatchString(endpoint) {
		t.Errorf("endpoint %q does not match expected pattern x.x.x.x:port", endpoint)
	}
	t.Logf("Discovered endpoint: %s", endpoint)
}
