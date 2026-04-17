package wireguard

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"unsafe"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	// TunnelIPClient is the WireGuard tunnel IP for the CLI side.
	TunnelIPClient = "10.0.0.1/30"
	// TunnelIPGateway is the WireGuard tunnel IP for the gateway side.
	TunnelIPGateway = "10.0.0.2/30"
	// PersistentKeepalive in seconds — keeps NAT mappings alive (< Cloud NAT 30s timeout).
	PersistentKeepalive = 20
)

// Device wraps a userspace WireGuard device with an associated netstack.
type Device struct {
	dev *device.Device
	net *netstack.Net
	tun tun.Device
}

type netstackView struct {
	_     unsafe.Pointer
	stack *stack.Stack
}

// NewDevice creates a userspace WireGuard device using wireguard-go + gVisor netstack.
// privateKey: this peer's WireGuard private key
// peerPublicKey: the other peer's WireGuard public key
// endpoint: the other peer's UDP endpoint (ip:port)
// listenPort: local UDP port to listen on (0 for OS-assigned)
// localIP: the tunnel IP for this peer (e.g., TunnelIPClient or TunnelIPGateway)
func NewDevice(privateKey wgtypes.Key, peerPublicKey wgtypes.Key, endpoint string, listenPort int, localIP string) (*Device, error) {
	return newDevice(privateKey, peerPublicKey, endpoint, conn.NewDefaultBind(), fmt.Sprintf("listen_port=%d\n", listenPort), localIP)
}

func newDevice(privateKey wgtypes.Key, peerPublicKey wgtypes.Key, endpoint string, bind conn.Bind, listenPortConfig string, localIP string) (*Device, error) {
	prefix, err := netip.ParsePrefix(localIP)
	if err != nil {
		return nil, fmt.Errorf("parse local IP %q: %w", localIP, err)
	}

	tun, net, err := netstack.CreateNetTUN(
		[]netip.Addr{prefix.Addr()},
		[]netip.Addr{}, // no DNS
		1420,           // MTU
	)
	if err != nil {
		return nil, fmt.Errorf("create netstack TUN: %w", err)
	}
	if bind == nil {
		return nil, fmt.Errorf("wireguard bind is nil")
	}

	logger := device.NewLogger(device.LogLevelError, "[wireguard] ")
	dev := device.NewDevice(tun, bind, logger)

	// Configure WireGuard: set private key and peer.
	cfg := fmt.Sprintf(`private_key=%s
%spublic_key=%s
persistent_keepalive_interval=%d
allowed_ip=0.0.0.0/0
`, encodeKey(privateKey), listenPortConfig, encodeKey(peerPublicKey), PersistentKeepalive)

	if endpoint != "" {
		cfg += fmt.Sprintf("endpoint=%s\n", endpoint)
	}

	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure wireguard device: %w", err)
	}

	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("bring up wireguard device: %w", err)
	}

	return &Device{dev: dev, net: net, tun: tun}, nil
}

// Net returns the netstack network for creating TCP connections through the tunnel.
func (d *Device) Net() *netstack.Net {
	return d.net
}

// Stack returns the underlying gVisor network stack.
func (d *Device) Stack() *stack.Stack {
	if d == nil || d.net == nil {
		return nil
	}
	return (*netstackView)(unsafe.Pointer(d.net)).stack
}

// DialTCP creates a TCP connection through the WireGuard tunnel.
func (d *Device) DialTCP(addr string) (net.Conn, error) {
	return d.net.Dial("tcp", addr)
}

// Close shuts down the WireGuard device cleanly.
func (d *Device) Close() {
	if d == nil || d.dev == nil {
		return
	}
	d.dev.Close()
}

func encodeKey(key wgtypes.Key) string {
	return hex.EncodeToString(key[:])
}
