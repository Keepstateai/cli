// KS-033: the size reports are debounced, bounded, never repeated for an
// unchanged size, and stop for good once the lease no longer controls the
// agent.
package main

import (
	"sync"
	"testing"
	"time"
)

func TestSizeReportsAreDebouncedBoundedAndStopWithTheLease(t *testing.T) {
	var mu sync.Mutex
	var sent [][2]int
	size := [2]int{120, 40}
	var refuse error
	r := &sizeReporter{delay: 20 * time.Millisecond,
		measure: func() (int, int) { mu.Lock(); defer mu.Unlock(); return size[0], size[1] },
		send: func(c, rw int) error {
			mu.Lock()
			defer mu.Unlock()
			if refuse != nil {
				return refuse
			}
			sent = append(sent, [2]int{c, rw})
			return nil
		}}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(sent) }
	r.now() // at open
	if count() != 1 {
		t.Fatalf("open: %v", sent)
	}
	// a drag: many resizes, one report once it settles
	for i := 0; i < 30; i++ {
		mu.Lock()
		size = [2]int{100 + i, 40}
		mu.Unlock()
		r.resized()
		time.Sleep(time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	if count() != 2 || sent[1] != [2]int{129, 40} {
		t.Fatalf("debounce: %v", sent)
	}
	// the same size again is not re-sent; an impossible size is not sent
	r.now()
	mu.Lock()
	size = [2]int{5, 2}
	mu.Unlock()
	r.now()
	if count() != 2 {
		t.Fatalf("repeat/invalid: %v", sent)
	}
	// the lease is gone: reports stop for good
	mu.Lock()
	size, refuse = [2]int{90, 30}, &hostedErr{Type: "ks_lease_gone"}
	mu.Unlock()
	r.now()
	mu.Lock()
	refuse = nil
	mu.Unlock()
	r.resized()
	time.Sleep(60 * time.Millisecond)
	r.now()
	if count() != 2 {
		t.Fatalf("after the lease went: %v", sent)
	}
}
