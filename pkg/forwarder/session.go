package forwarder

import (
	"net"
	"sync"
	"time"
)

type Session struct {
	clientAddr   *net.UDPAddr
	upstreamConn *net.UDPConn

	mu           sync.RWMutex
	lastActivity time.Time
}

type SessionMap struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewSessionMap() *SessionMap {
	return &SessionMap{sessions: make(map[string]*Session)}
}

func (m *SessionMap) Get(key string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[key]
	return s, ok
}

func (m *SessionMap) Set(key string, s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[key] = s
}

func (m *SessionMap) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, key)
}

func (m *SessionMap) CleanupIdle(timeout time.Duration) {
	cutoff := time.Now().Add(-timeout)

	m.mu.Lock()
	defer m.mu.Unlock()

	for key, session := range m.sessions {
		if session.LastActivity().After(cutoff) {
			continue
		}

		_ = session.upstreamConn.Close()
		delete(m.sessions, key)
	}
}

func (m *SessionMap) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, session := range m.sessions {
		_ = session.upstreamConn.Close()
		delete(m.sessions, key)
	}
}

func NewSession(clientAddr *net.UDPAddr, upstreamConn *net.UDPConn) *Session {
	return &Session{
		clientAddr:   clientAddr,
		upstreamConn: upstreamConn,
		lastActivity: time.Now(),
	}
}

func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastActivity = time.Now()
}

func (s *Session) LastActivity() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.lastActivity
}
