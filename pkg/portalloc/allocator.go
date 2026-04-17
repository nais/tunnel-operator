package portalloc

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

const (
	defaultMinPort int32 = 10000
	defaultMaxPort int32 = 60000
)

var errRangeExhausted = errors.New("port range exhausted")

type PortAllocator struct {
	mu        sync.Mutex
	minPort   int32
	maxPort   int32
	allocated map[string]int32
	used      map[int32]string
	rng       *rand.Rand
}

func New(minPort, maxPort int32) *PortAllocator {
	if minPort <= 0 {
		minPort = defaultMinPort
	}
	if maxPort <= 0 {
		maxPort = defaultMaxPort
	}
	if maxPort < minPort {
		minPort = defaultMinPort
		maxPort = defaultMaxPort
	}

	return &PortAllocator{
		minPort:   minPort,
		maxPort:   maxPort,
		allocated: make(map[string]int32),
		used:      make(map[int32]string),
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (a *PortAllocator) Allocate(tunnelKey string) (int32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if port, ok := a.allocated[tunnelKey]; ok {
		return port, nil
	}

	rangeSize := int(a.maxPort-a.minPort) + 1
	start := a.rng.Intn(rangeSize)

	for offset := range rangeSize {
		port := a.minPort + int32((start+offset)%rangeSize)
		if _, ok := a.used[port]; ok {
			continue
		}

		a.allocated[tunnelKey] = port
		a.used[port] = tunnelKey
		return port, nil
	}

	return 0, errRangeExhausted
}

func (a *PortAllocator) Release(tunnelKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	port, ok := a.allocated[tunnelKey]
	if !ok {
		return
	}

	delete(a.allocated, tunnelKey)
	delete(a.used, port)
}

func (a *PortAllocator) IsAllocated(port int32) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.used[port]
	return ok
}

func (a *PortAllocator) GetPort(tunnelKey string) (int32, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	port, ok := a.allocated[tunnelKey]
	return port, ok
}

func (a *PortAllocator) LoadExisting(tunnelKey string, port int32) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if port < a.minPort || port > a.maxPort {
		return
	}

	if currentKey, ok := a.used[port]; ok && currentKey != tunnelKey {
		return
	}

	if existingPort, ok := a.allocated[tunnelKey]; ok && existingPort != port {
		delete(a.used, existingPort)
	}

	a.allocated[tunnelKey] = port
	a.used[port] = tunnelKey
}
