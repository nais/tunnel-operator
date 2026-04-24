package wireguard

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"unsafe"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
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
// logger: structured logger for WireGuard device logs (nil for silent)
func NewDevice(
	privateKey wgtypes.Key, peerPublicKey wgtypes.Key,
	endpoint string, listenPort int, localIP string, logger *slog.Logger,
) (*Device, error) {
	return newDevice(
		privateKey, peerPublicKey, endpoint,
		conn.NewDefaultBind(), fmt.Sprintf("listen_port=%d\n", listenPort), localIP, logger,
	)
}

func newDevice(
	privateKey wgtypes.Key, peerPublicKey wgtypes.Key,
	endpoint string, bind conn.Bind, listenPortConfig string, localIP string, logger *slog.Logger,
) (*Device, error) {
	prefix, err := netip.ParsePrefix(localIP)
	if err != nil {
		return nil, fmt.Errorf("parse local IP %q: %w", localIP, err)
	}

	tun, net, err := netstack.CreateNetTUN(
		[]netip.Addr{prefix.Addr()},
		[]netip.Addr{},
		1420,
	)
	if err != nil {
		return nil, fmt.Errorf("create netstack TUN: %w", err)
	}
	if bind == nil {
		return nil, fmt.Errorf("wireguard bind is nil")
	}

	wgLogger := slogDeviceLogger(logger)
	dev := device.NewDevice(tun, bind, wgLogger)

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

	d := &Device{dev: dev, net: net, tun: tun}
	tuneStack(d.Stack())
	return d, nil
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

// tuneStack adjusts gVisor netstack TCP parameters for better throughput.
// Buffer max sizes match Tailscale's production values (tailscale/tailscale#12994).
// RACK is disabled due to a gVisor bug that causes spurious retransmissions
// and congestion window collapse (tailscale/tailscale#9707).
func tuneStack(s *stack.Stack) {
	if s == nil {
		return
	}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPReceiveBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,
		Default: tcp.DefaultReceiveBufferSize,
		Max:     8 << 20, // 8MiB
	})
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPSendBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,
		Default: tcp.DefaultSendBufferSize,
		Max:     6 << 20, // 6MiB
	})
	rackOpt := tcpip.TCPRecovery(0)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rackOpt)
}

func slogDeviceLogger(logger *slog.Logger) *device.Logger {
	if logger == nil {
		return device.NewLogger(device.LogLevelSilent, "")
	}
	wg := logger.WithGroup("wireguard")
	return &device.Logger{
		Verbosef: func(format string, args ...any) {
			wg.Debug(fmt.Sprintf(format, args...))
		},
		Errorf: func(format string, args ...any) {
			wg.Error(fmt.Sprintf(format, args...))
		},
	}
}
