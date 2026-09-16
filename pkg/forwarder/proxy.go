package forwarder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
)

const udpBufferSize = 65535

type PortMapping struct {
	GatewayAddr string
}

type UDPProxy struct {
	mu          sync.RWMutex
	mappings    map[int]*portListener
	idleTimeout time.Duration
	vip         string
}

type portListener struct {
	conn            *net.UDPConn
	gateway         string
	revision        int64
	tunnelName      string
	tunnelNamespace string
	sessions        *SessionMap
	cancel          context.CancelFunc
}

func NewUDPProxy(idleTimeout time.Duration) *UDPProxy {
	return &UDPProxy{
		mappings:    make(map[int]*portListener),
		idleTimeout: idleTimeout,
	}
}

func (p *UDPProxy) AddMapping(ctx context.Context, port int, gatewayAddr, tunnelName, tunnelNamespace string) error {
	return p.addMapping(ctx, port, gatewayAddr, tunnelName, tunnelNamespace, 0)
}

// ApplyMapping applies a mapping only when it is at least as recent as the
// active mapping. Revisions make queued updates from before a snapshot safe to
// replay after that snapshot has been reconciled.
func (p *UDPProxy) ApplyMapping(ctx context.Context, mapping *forwarderv1.TunnelMapping) error {
	if mapping == nil || mapping.GetForwarderPort() <= 0 || mapping.GetGatewayAddress() == "" {
		return fmt.Errorf("invalid tunnel mapping")
	}
	return p.addMapping(
		ctx,
		int(mapping.GetForwarderPort()),
		mapping.GetGatewayAddress(),
		mapping.GetTunnelName(),
		mapping.GetTunnelNamespace(),
		mapping.GetRevision(),
	)
}

func (p *UDPProxy) addMapping(
	ctx context.Context, port int, gatewayAddr, tunnelName, tunnelNamespace string, revision int64,
) error {
	p.mu.RLock()
	existing, exists := p.mappings[port]
	p.mu.RUnlock()
	if exists {
		sameTunnel := existing.tunnelName == tunnelName && existing.tunnelNamespace == tunnelNamespace
		if sameTunnel && existing.revision > revision {
			return nil
		}
		if sameTunnel && existing.gateway == gatewayAddr {
			p.mu.Lock()
			if current, ok := p.mappings[port]; ok && current == existing && current.revision < revision {
				current.revision = revision
			}
			p.mu.Unlock()
			return nil
		}
		p.RemoveMapping(port)
	}

	listenerAddr := &net.UDPAddr{Port: port}
	conn, err := net.ListenUDP("udp4", listenerAddr)
	if err != nil {
		return fmt.Errorf("listen udp on port %d: %w", port, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	pl := &portListener{
		conn:            conn,
		gateway:         gatewayAddr,
		revision:        revision,
		tunnelName:      tunnelName,
		tunnelNamespace: tunnelNamespace,
		sessions:        NewSessionMap(),
		cancel:          cancel,
	}

	p.mu.Lock()
	if _, exists := p.mappings[port]; exists {
		p.mu.Unlock()
		cancel()
		_ = conn.Close()
		return fmt.Errorf("mapping already exists for port %d", port)
	}
	p.mappings[port] = pl
	p.mu.Unlock()

	slog.Info("listening on port",
		"port", port, "gateway", gatewayAddr,
		"tunnel", tunnelName, "namespace", tunnelNamespace,
	)
	go p.servePort(ctx, pl)
	go p.cleanupIdleSessions(ctx, pl)

	return nil
}

// ReconcileMappings makes mappings exactly match the authoritative snapshot.
// It applies all snapshot mappings before removing stale listeners so a failed
// replacement never removes an otherwise usable mapping.
func (p *UDPProxy) ReconcileMappings(ctx context.Context, mappings []*forwarderv1.TunnelMapping) error {
	desired := make(map[int]struct{}, len(mappings))
	for _, mapping := range mappings {
		if mapping == nil || mapping.GetForwarderPort() <= 0 || mapping.GetGatewayAddress() == "" {
			continue
		}
		port := int(mapping.GetForwarderPort())
		desired[port] = struct{}{}
		if err := p.ApplyMapping(ctx, mapping); err != nil {
			return fmt.Errorf("apply mapping for port %d: %w", port, err)
		}
	}

	for _, port := range p.Ports() {
		if _, ok := desired[port]; !ok {
			p.RemoveMapping(port)
		}
	}
	return nil
}

// Ports returns a snapshot of active listener ports.
func (p *UDPProxy) Ports() []int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ports := make([]int, 0, len(p.mappings))
	for port := range p.mappings {
		ports = append(ports, port)
	}
	return ports
}

func (p *UDPProxy) RemoveMapping(port int) {
	p.mu.Lock()
	pl, ok := p.mappings[port]
	if ok {
		delete(p.mappings, port)
	}
	p.mu.Unlock()
	if !ok {
		return
	}

	pl.cancel()
	_ = pl.conn.Close()
	pl.sessions.CloseAll()
}

func (p *UDPProxy) Close() {
	p.mu.Lock()
	ports := make([]int, 0, len(p.mappings))
	for port := range p.mappings {
		ports = append(ports, port)
	}
	p.mu.Unlock()

	for _, port := range ports {
		p.RemoveMapping(port)
	}
}

func (p *UDPProxy) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.Close()
	return nil
}

func (p *UDPProxy) SetVIP(vip string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.vip = vip
}

func (p *UDPProxy) servePort(ctx context.Context, listener *portListener) {
	buf := make([]byte, udpBufferSize)

	for {
		n, clientAddr, err := listener.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}

		port := listener.conn.LocalAddr().(*net.UDPAddr).Port
		slog.Debug("received UDP packet",
			"port", port, "from", clientAddr.String(), "bytes", n,
		)

		session, err := p.getOrCreateSession(listener, clientAddr)
		if err != nil {
			slog.Error("failed to create session",
				"port", port, "from", clientAddr.String(),
				"gateway", listener.gateway, "err", err,
			)
			continue
		}

		session.Touch()
		if _, err := session.upstreamConn.Write(buf[:n]); err != nil {
			slog.Error("failed to forward to gateway",
				"port", port, "gateway", listener.gateway, "err", err,
			)
			listener.sessions.Delete(clientAddr.String())
			_ = session.upstreamConn.Close()
		} else {
			slog.Debug("forwarded to gateway",
				"port", port, "gateway", listener.gateway, "bytes", n,
			)
			PacketsTotal.WithLabelValues(listener.tunnelName, listener.tunnelNamespace, DirectionClientToGateway).Inc()
			BytesTotal.WithLabelValues(listener.tunnelName, listener.tunnelNamespace, DirectionClientToGateway).Add(float64(n))
		}
	}
}

func (p *UDPProxy) getOrCreateSession(listener *portListener, clientAddr *net.UDPAddr) (*Session, error) {
	key := clientAddr.String()
	if session, ok := listener.sessions.Get(key); ok {
		return session, nil
	}

	upstreamAddr, err := net.ResolveUDPAddr("udp4", listener.gateway)
	if err != nil {
		return nil, fmt.Errorf("resolve gateway %q: %w", listener.gateway, err)
	}

	upstreamConn, err := net.DialUDP("udp4", nil, upstreamAddr)
	if err != nil {
		return nil, fmt.Errorf("dial gateway %q: %w", listener.gateway, err)
	}

	slog.Info("new session",
		"port", listener.conn.LocalAddr().(*net.UDPAddr).Port,
		"client", clientAddr.String(),
		"gateway", listener.gateway, "tunnel", listener.tunnelName,
	)

	session := NewSession(clientAddr, upstreamConn)
	listener.sessions.Set(key, session)
	ActiveSessions.WithLabelValues(listener.tunnelName, listener.tunnelNamespace).Inc()
	go p.pipeResponses(listener, key, session)

	return session, nil
}

func (p *UDPProxy) pipeResponses(listener *portListener, key string, session *Session) {
	defer ActiveSessions.WithLabelValues(listener.tunnelName, listener.tunnelNamespace).Dec()
	buf := make([]byte, udpBufferSize)

	for {
		n, err := session.upstreamConn.Read(buf)
		if err != nil {
			slog.Debug("gateway read error", "tunnel", listener.tunnelName, "client", session.clientAddr.String(), "err", err)
			listener.sessions.Delete(key)
			_ = session.upstreamConn.Close()
			return
		}

		session.Touch()
		if _, err := listener.conn.WriteToUDP(buf[:n], session.clientAddr); err != nil {
			slog.Error("failed to send response to client",
				"tunnel", listener.tunnelName,
				"client", session.clientAddr.String(), "err", err,
			)
			listener.sessions.Delete(key)
			_ = session.upstreamConn.Close()
			return
		}
		slog.Debug("forwarded response to client",
			"tunnel", listener.tunnelName,
			"client", session.clientAddr.String(), "bytes", n,
		)
		PacketsTotal.WithLabelValues(listener.tunnelName, listener.tunnelNamespace, DirectionGatewayToClient).Inc()
		BytesTotal.WithLabelValues(listener.tunnelName, listener.tunnelNamespace, DirectionGatewayToClient).Add(float64(n))
	}
}

func (p *UDPProxy) cleanupIdleSessions(ctx context.Context, listener *portListener) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			listener.sessions.CleanupIdle(p.idleTimeout)
		}
	}
}
