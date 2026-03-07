package gc

import (
	"math/rand"
	"sync"
	"time"
)

// ConcurrentHeap wraps a Heap with a mutex so mutators and the GC
// can share it safely. The mutex protects data races on the map, but
// there is deliberately NO write barrier — the tri-color invariant
// will be violated.
type ConcurrentHeap struct {
	mu   sync.Mutex
	Heap *Heap
}

func NewConcurrentHeap(maxSize int) *ConcurrentHeap {
	return &ConcurrentHeap{Heap: NewHeap(maxSize)}
}

// ConcurrentMarkSweep runs mark-sweep while releasing the lock between
// individual steps, giving mutators a chance to interleave and break
// the tri-color invariant.
func ConcurrentMarkSweep(ch *ConcurrentHeap, rootIDs []int) {
	// Phase 1: Reset all objects to white.
	ch.mu.Lock()
	for _, obj := range ch.Heap.Objects {
		obj.Color = White
	}
	ch.mu.Unlock()

	// Phase 2: Shade roots grey.
	ch.mu.Lock()
	greySet := make([]*Object, 0)
	for _, id := range rootIDs {
		if obj, ok := ch.Heap.Objects[id]; ok {
			obj.Color = Grey
			greySet = append(greySet, obj)
		}
	}
	ch.mu.Unlock()

	// Phase 3: Process one grey object per lock acquisition.
	// This is where the bug lives: between the unlock after scanning
	// an object (now black) and the next lock, a mutator can rewire
	// a pointer from that black object to a white object. The GC will
	// never discover the white object because the black source won't
	// be scanned again.
	for {
		ch.mu.Lock()
		if len(greySet) == 0 {
			ch.mu.Unlock()
			break
		}
		current := greySet[0]
		greySet = greySet[1:]

		for _, child := range current.Children {
			if child.Color == White {
				child.Color = Grey
				greySet = append(greySet, child)
			}
		}
		current.Color = Black
		ch.mu.Unlock()

		// Yield to let mutators run and rewire pointers.
		time.Sleep(time.Microsecond)
	}

	// Phase 4: Sweep all remaining white objects.
	ch.mu.Lock()
	for id, obj := range ch.Heap.Objects {
		if obj.Color == White {
			delete(ch.Heap.Objects, id)
		}
	}
	ch.mu.Unlock()
}

// Mutator simulates a running goroutine that continuously allocates and
// rewires pointers. It stops when done is closed.
func Mutator(ch *ConcurrentHeap, rootIDs []int, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for {
		select {
		case <-done:
			return
		default:
		}

		ch.mu.Lock()

		// Allocate a new object if there's room.
		obj, err := ch.Heap.Alloc()
		if err == nil {
			// Attach the new object to a random existing object so it's
			// reachable (sometimes). This creates the scenario where a
			// black object gains a pointer to a brand-new white object.
			ids := heapIDs(ch.Heap)
			if len(ids) > 1 {
				parentID := ids[rng.Intn(len(ids))]
				parent := ch.Heap.Objects[parentID]
				parent.Children = append(parent.Children, obj)
			}
		}

		// Rewire: pick a random object and point it at another random object.
		// This is the critical mutation: if the source is black and the
		// target is white, and no grey object still references the target,
		// the GC will miss the target and incorrectly sweep it.
		ids := heapIDs(ch.Heap)
		if len(ids) >= 2 {
			srcID := ids[rng.Intn(len(ids))]
			dstID := ids[rng.Intn(len(ids))]
			src := ch.Heap.Objects[srcID]
			dst := ch.Heap.Objects[dstID]
			if src != dst {
				// Replace all children with a single pointer to dst.
				// This is aggressive — it maximizes the chance of
				// severing the old grey-path to dst while creating
				// a new black→white edge.
				src.Children = []*Object{dst}
			}
		}

		ch.mu.Unlock()
		time.Sleep(time.Duration(rng.Intn(500)) * time.Microsecond)
	}
}

// heapIDs returns all live object IDs (for random selection).
func heapIDs(h *Heap) []int {
	ids := make([]int, 0, len(h.Objects))
	for id := range h.Objects {
		ids = append(ids, id)
	}
	return ids
}
