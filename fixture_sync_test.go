// fixture_sync_test.go: the synchronization between a recording fake
// control plane and the test that reads what it recorded (third-party
// review R01).
//
// A fake's handler runs on the HTTP server's goroutines and records under
// the fake's mutex; the test drives the client as a separate process and
// reads the record once the process has exited. The process boundary is
// not a happens-before edge the race detector can see, so an unlocked read
// is rejected by -race even though the handler finished first. A fake
// registers its mutex with syncFixture; every run of the client
// (auditExec, ksIn) then passes through it before the client starts and
// after it exits. Lock-and-release after exit waits for any handler still
// finishing and orders all it recorded before the test's reads; the same
// before the start orders the test's own changes (a fault it switches on)
// before the next request is handled.
package main

import (
	"sync"
	"testing"
)

var fixtureLocks struct {
	sync.Mutex
	m map[*sync.Mutex]bool
}

// syncFixture registers a fake's mutex for the rest of the test.
func syncFixture(t *testing.T, mu *sync.Mutex) {
	t.Helper()
	fixtureLocks.Lock()
	if fixtureLocks.m == nil {
		fixtureLocks.m = map[*sync.Mutex]bool{}
	}
	fixtureLocks.m[mu] = true
	fixtureLocks.Unlock()
	t.Cleanup(func() {
		fixtureLocks.Lock()
		delete(fixtureLocks.m, mu)
		fixtureLocks.Unlock()
	})
}

// fixtureBarrier passes through every registered fake's mutex.
func fixtureBarrier() {
	fixtureLocks.Lock()
	mus := make([]*sync.Mutex, 0, len(fixtureLocks.m))
	for mu := range fixtureLocks.m {
		mus = append(mus, mu)
	}
	fixtureLocks.Unlock()
	for _, mu := range mus {
		mu.Lock()
		mu.Unlock() //nolint:staticcheck // an empty critical section is the point: an ordering edge
	}
}
