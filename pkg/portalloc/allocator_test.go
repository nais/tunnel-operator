package portalloc

import (
	"math/rand"
	"testing"
)

func TestAllocateReusesExistingPortForTunnel(t *testing.T) {
	t.Parallel()

	a := New(10000, 10010)
	first, err := a.Allocate("default/tunnel-a")
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}

	second, err := a.Allocate("default/tunnel-a")
	if err != nil {
		t.Fatalf("Allocate() second call error = %v", err)
	}

	if first != second {
		t.Fatalf("expected same port, got %d and %d", first, second)
	}
	if !a.IsAllocated(first) {
		t.Fatalf("expected port %d to be allocated", first)
	}
}

func TestReleaseFreesPort(t *testing.T) {
	t.Parallel()

	a := New(10000, 10010)
	port, err := a.Allocate("default/tunnel-a")
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}

	a.Release("default/tunnel-a")

	if a.IsAllocated(port) {
		t.Fatalf("expected port %d to be released", port)
	}
	if _, ok := a.GetPort("default/tunnel-a"); ok {
		t.Fatalf("expected tunnel allocation to be removed")
	}
}

func TestAllocateFailsWhenRangeExhausted(t *testing.T) {
	t.Parallel()

	a := New(10000, 10001)
	if _, err := a.Allocate("default/tunnel-a"); err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if _, err := a.Allocate("default/tunnel-b"); err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if _, err := a.Allocate("default/tunnel-c"); err == nil {
		t.Fatalf("expected exhaustion error")
	}
}

func TestAllocateSkipsAlreadyUsedPorts(t *testing.T) {
	t.Parallel()

	a := New(10000, 10001)
	a.rng = rand.New(rand.NewSource(1))
	a.LoadExisting("default/tunnel-a", 10000)

	port, err := a.Allocate("default/tunnel-b")
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}

	if port != 10001 {
		t.Fatalf("expected allocator to skip used port and pick 10001, got %d", port)
	}
}

func TestLoadExistingRestoresAllocation(t *testing.T) {
	t.Parallel()

	a := New(10000, 10010)
	a.LoadExisting("default/tunnel-a", 10005)

	port, ok := a.GetPort("default/tunnel-a")
	if !ok {
		t.Fatalf("expected tunnel allocation to exist")
	}
	if port != 10005 {
		t.Fatalf("expected port 10005, got %d", port)
	}
	if !a.IsAllocated(10005) {
		t.Fatalf("expected port 10005 to be marked allocated")
	}
}

func TestLoadExistingIgnoresConflictingPort(t *testing.T) {
	t.Parallel()

	a := New(10000, 10010)
	a.LoadExisting("default/tunnel-a", 10005)
	a.LoadExisting("default/tunnel-b", 10005)

	port, ok := a.GetPort("default/tunnel-b")
	if ok {
		t.Fatalf("expected conflicting allocation to be ignored, got port %d", port)
	}

	port, ok = a.GetPort("default/tunnel-a")
	if !ok || port != 10005 {
		t.Fatalf("expected original allocation to be preserved, got %d, exists=%v", port, ok)
	}
}
