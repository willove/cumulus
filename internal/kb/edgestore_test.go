package kb

import (
	"fmt"
	"sync"
	"testing"

	"github.com/willove/cumulus/internal/graph"
)

// storeID gives a comparable identity for a graph.Store so the test can tell
// "same store" from "a second materialisation".
func storeID(st graph.Store) string {
	return fmt.Sprintf("%p", st)
}

func newTestMemoryStore() graph.Store { return graph.NewMemory() }

// edgeStore used to be an unguarded lazy init reachable from three write
// paths that a serving process enters concurrently. Two first-writers could
// each build a store and one would be silently discarded — taking every edge
// that writer had added with it. sync.Once makes the materialisation happen
// exactly once no matter how many goroutines arrive.

func TestEdgeStoreMaterialisesExactlyOnceUnderConcurrency(t *testing.T) {
	e := &Engine{} // Edges nil → the lazy path is what is under test

	const goroutines = 64
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		firstID string
		dupes   int
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			st := e.edgeStore()
			if st == nil {
				t.Error("edgeStore returned nil")
				return
			}
			// Compare interface identity: a second materialisation would be a
			// different underlying *graph.Memory pointer.
			mu.Lock()
			defer mu.Unlock()
			if firstID == "" {
				firstID = storeID(st)
			} else if storeID(st) != firstID {
				dupes++
			}
		}()
	}
	wg.Wait()

	if firstID == "" {
		t.Fatal("no store was ever produced")
	}
	if dupes != 0 {
		t.Fatalf("%d of %d goroutines got a DIFFERENT store — the lazy init raced "+
			"and one materialisation was discarded", dupes, goroutines)
	}
	if e.Edges == nil {
		t.Fatal("Edges was never published onto the engine")
	}
}

// The same guarantee for the explicit-store case: an Engine constructed with a
// store must keep exactly that store, not replace it.
func TestEdgeStoreKeepsAnExplicitStore(t *testing.T) {
	explicit := newTestMemoryStore()
	e := &Engine{Edges: explicit}

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if got := e.edgeStore(); storeID(got) != storeID(explicit) {
				t.Errorf("edgeStore replaced the engine's explicit store")
			}
		}()
	}
	wg.Wait()
}
