package gc

import (
	"math/rand"
	"sync"
	"time"
)

// ConcurrentHeap wraps a Heap with a mutex so mutators and the GC
// can share it safely.
type ConcurrentHeap struct {
	mu   sync.Mutex
	Heap *Heap
}

func NewConcurrentHeap(maxSize int) *ConcurrentHeap {
	return &ConcurrentHeap{Heap: NewHeap(maxSize)}
}

// --- Unsafe collector (no write barrier) — preserved from Phase 2 ---

// ConcurrentMarkSweepUnsafe runs mark-sweep WITHOUT a write barrier.
// The tri-color invariant WILL be violated.
func ConcurrentMarkSweepUnsafe(ch *ConcurrentHeap, rootIDs []int) {
	ch.mu.Lock()
	for _, obj := range ch.Heap.Objects {
		obj.Color = White
	}
	ch.mu.Unlock()

	ch.mu.Lock()
	greySet := make([]*Object, 0)
	for _, id := range rootIDs {
		if obj, ok := ch.Heap.Objects[id]; ok {
			obj.Color = Grey
			greySet = append(greySet, obj)
		}
	}
	ch.mu.Unlock()

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
		time.Sleep(time.Microsecond)
	}

	ch.mu.Lock()
	for id, obj := range ch.Heap.Objects {
		if obj.Color == White {
			delete(ch.Heap.Objects, id)
		}
	}
	ch.mu.Unlock()
}

// --- Safe collector (with hybrid write barrier) ---

// ConcurrentMarkSweep runs mark-sweep with the hybrid write barrier
// enabled during the mark phase. Mutators that call ReplaceChildren /
// SetChild will fire WriteBarrier, which shades both the old and new
// targets grey — preventing the lost-object problem from Phase 2.
func ConcurrentMarkSweep(ch *ConcurrentHeap, rootIDs []int) {
	// Phase 1: Reset all objects to white and enable the barrier.
	ch.mu.Lock()
	for _, obj := range ch.Heap.Objects {
		obj.Color = White
	}
	ch.Heap.Marking = true // barrier is now active
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

	// Phase 3: Process grey objects one at a time, releasing the lock
	// between each to let mutators interleave.
	//
	// Key difference from Phase 2: when a mutator rewires a pointer,
	// the write barrier shades the targets grey. Those grey objects
	// won't be in our local greySet yet, so each iteration we also
	// scan the heap for any objects the barrier shaded grey.
	for {
		ch.mu.Lock()

		// Collect any objects the barrier shaded grey since our last pass.
		// This is how the GC "learns" about pointer writes it missed.
		for _, obj := range ch.Heap.Objects {
			if obj.Color == Grey {
				alreadyQueued := false
				for _, g := range greySet {
					if g == obj {
						alreadyQueued = true
						break
					}
				}
				if !alreadyQueued {
					greySet = append(greySet, obj)
				}
			}
		}

		if len(greySet) == 0 {
			ch.mu.Unlock()
			break
		}

		current := greySet[0]
		greySet = greySet[1:]

		// Skip if already processed (another path may have turned it black).
		if current.Color != Grey {
			ch.mu.Unlock()
			continue
		}

		for _, child := range current.Children {
			if child.Color == White {
				child.Color = Grey
				greySet = append(greySet, child)
			}
		}
		current.Color = Black
		ch.mu.Unlock()

		time.Sleep(time.Microsecond)
	}

	// Phase 4: Disable the barrier and sweep.
	ch.mu.Lock()
	ch.Heap.Marking = false
	for id, obj := range ch.Heap.Objects {
		if obj.Color == White {
			delete(ch.Heap.Objects, id)
		}
	}
	ch.mu.Unlock()
}

// --- Mutator (with write barrier) ---

// Mutator simulates a running goroutine that continuously allocates and
// rewires pointers. Uses ReplaceChildren so the write barrier fires on
// every pointer store.
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
			ids := heapIDs(ch.Heap)
			if len(ids) > 1 {
				parentID := ids[rng.Intn(len(ids))]
				parent := ch.Heap.Objects[parentID]
				// Use ReplaceChildren so the barrier fires.
				newChildren := make([]*Object, len(parent.Children)+1)
				copy(newChildren, parent.Children)
				newChildren[len(parent.Children)] = obj
				ReplaceChildren(ch.Heap, parent, newChildren)
			}
		}

		// Rewire: pick a random object and point it at another random object.
		ids := heapIDs(ch.Heap)
		if len(ids) >= 2 {
			srcID := ids[rng.Intn(len(ids))]
			dstID := ids[rng.Intn(len(ids))]
			src := ch.Heap.Objects[srcID]
			dst := ch.Heap.Objects[dstID]
			if src != dst {
				// Use ReplaceChildren — fires barrier for every old
				// child being dropped and the new child being added.
				ReplaceChildren(ch.Heap, src, []*Object{dst})
			}
		}

		ch.mu.Unlock()
		time.Sleep(time.Duration(rng.Intn(500)) * time.Microsecond)
	}
}

// MutatorUnsafe is the Phase 2 mutator — no write barrier.
func MutatorUnsafe(ch *ConcurrentHeap, rootIDs []int, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for {
		select {
		case <-done:
			return
		default:
		}

		ch.mu.Lock()

		obj, err := ch.Heap.Alloc()
		if err == nil {
			ids := heapIDs(ch.Heap)
			if len(ids) > 1 {
				parentID := ids[rng.Intn(len(ids))]
				parent := ch.Heap.Objects[parentID]
				parent.Children = append(parent.Children, obj)
			}
		}

		ids := heapIDs(ch.Heap)
		if len(ids) >= 2 {
			srcID := ids[rng.Intn(len(ids))]
			dstID := ids[rng.Intn(len(ids))]
			src := ch.Heap.Objects[srcID]
			dst := ch.Heap.Objects[dstID]
			if src != dst {
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
