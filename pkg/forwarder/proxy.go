package forwarder

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
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
	p.mu.RLock()
	existing, exists := p.mappings[port]
	p.mu.RUnlock()
	if exists {
		if existing.gateway == gatewayAddr {
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

	go p.servePort(ctx, pl)
	go p.cleanupIdleSessions(ctx, pl)

	return nil
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

		session, err := p.getOrCreateSession(listener, clientAddr)
		if err != nil {
			continue
		}

		session.Touch()
		if _, err := session.upstreamConn.Write(buf[:n]); err != nil {
			listener.sessions.Delete(clientAddr.String())
			_ = session.upstreamConn.Close()
		} else {
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
			listener.sessions.Delete(key)
			_ = session.upstreamConn.Close()
			return
		}

		session.Touch()
		if _, err := listener.conn.WriteToUDP(buf[:n], session.clientAddr); err != nil {
			listener.sessions.Delete(key)
			_ = session.upstreamConn.Close()
			return
		}
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
