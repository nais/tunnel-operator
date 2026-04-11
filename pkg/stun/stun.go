package stun

import (
	"fmt"
	"net"
	"time"

	"github.com/pion/stun/v3"
)

// DefaultSTUNServers is the ordered list of public STUN servers to use.
// Cloudflare is primary (anycast, Oslo/Stockholm PoP), Google servers are fallback.
var DefaultSTUNServers = []string{
	"stun.cloudflare.com:3478",
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
	"stun2.l.google.com:19302",
}

// DiscoverEndpoint performs a STUN binding request to discover the external UDP endpoint.
// Returns the external IP:port as a string (e.g., "1.2.3.4:12345").
// Tries each server in order; returns error if all fail.
func DiscoverEndpoint(conn *net.UDPConn, stunServers []string) (string, error) {
	if len(stunServers) == 0 {
		stunServers = DefaultSTUNServers
	}

	var lastErr error
	for _, server := range stunServers {
		endpoint, err := discoverFromServer(conn, server)
		if err != nil {
			lastErr = fmt.Errorf("STUN server %s: %w", server, err)
			continue
		}
		return endpoint, nil
	}

	return "", fmt.Errorf("all STUN servers failed (NAT traversal unavailable — check your network): %w", lastErr)
}

func discoverFromServer(conn *net.UDPConn, server string) (string, error) {
	serverAddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return "", fmt.Errorf("resolve STUN server address: %w", err)
	}

	message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", fmt.Errorf("set deadline: %w", err)
	}
	defer conn.SetDeadline(time.Time{})

	if _, err := conn.WriteTo(message.Raw, serverAddr); err != nil {
		return "", fmt.Errorf("send STUN request: %w", err)
	}

	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return "", fmt.Errorf("read STUN response: %w", err)
	}

	resp := &stun.Message{Raw: buf[:n]}
	if err := resp.Decode(); err != nil {
		return "", fmt.Errorf("decode STUN response: %w", err)
	}

	var xorAddr stun.XORMappedAddress
	if err := xorAddr.GetFrom(resp); err != nil {
		var mappedAddr stun.MappedAddress
		if err2 := mappedAddr.GetFrom(resp); err2 != nil {
			return "", fmt.Errorf("get mapped address from STUN response: %w", err)
		}
		return fmt.Sprintf("%s:%d", mappedAddr.IP.String(), mappedAddr.Port), nil
	}

	return fmt.Sprintf("%s:%d", xorAddr.IP.String(), xorAddr.Port), nil
}
