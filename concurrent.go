package gc

import (
	"math/rand"
	"sync"
	"time"
)

// ConcurrentHeap wraps a Heap with a mutex so mutators and the GC
// can share it safely. The GreyQueue is a shared work queue: the
// write barrier pushes grey objects here, and the GC pops from it.
// This replaces the O(n) full-heap scan from Phase 3.
type ConcurrentHeap struct {
	mu        sync.Mutex
	Heap      *Heap
	GreyQueue []*Object // shared work queue for barrier-greyed objects
}

func NewConcurrentHeap(maxSize int) *ConcurrentHeap {
	return &ConcurrentHeap{Heap: NewHeap(maxSize)}
}

// PushGrey adds an object to the shared grey work queue.
// Must be called while ch.mu is held.
func (ch *ConcurrentHeap) PushGrey(obj *Object) {
	ch.GreyQueue = append(ch.GreyQueue, obj)
}

// DrainGrey moves all objects from the shared grey queue into the
// caller's local work list and resets the queue. Must be called
// while ch.mu is held.
func (ch *ConcurrentHeap) DrainGrey() []*Object {
	drained := ch.GreyQueue
	ch.GreyQueue = nil
	return drained
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

// GCPhaseTimings records how long each GC phase took.
type GCPhaseTimings struct {
	STWMarkSetup   time.Duration // STW pause #1: reset + enable barrier + shade roots
	ConcurrentMark time.Duration // concurrent mark: trace objects (mutators run freely)
	STWSweep       time.Duration // STW pause #2: disable barrier + sweep
}

// ConcurrentMarkSweep runs mark-sweep with the hybrid write barrier
// enabled during the mark phase. Returns per-phase timings so callers
// can distinguish true STW pauses from concurrent work.
//
// Real Go's GC has the same three-phase structure:
//   STW #1 → concurrent mark → STW #2
func ConcurrentMarkSweep(ch *ConcurrentHeap, rootIDs []int) GCPhaseTimings {
	var timings GCPhaseTimings

	// ── STW PAUSE #1: Mark setup ─────────────────────────────────
	// All mutators are blocked (we hold the lock for the entire block).
	// In real Go, this is a brief stop-the-world where the runtime:
	//   - enables the write barrier
	//   - resets mark state
	//   - scans goroutine stacks to shade root pointers grey
	stwStart := time.Now()
	ch.mu.Lock()
	for _, obj := range ch.Heap.Objects {
		obj.Color = White
	}
	ch.Heap.Marking = true // write barrier is now active
	// Wire up the grey pusher: barrier-greyed objects go directly
	// into the shared work queue instead of being discovered by
	// scanning the entire heap. This is O(1) per barrier fire.
	ch.GreyQueue = nil
	ch.Heap.GreyPusher = func(obj *Object) {
		ch.PushGrey(obj)
	}
	greySet := make([]*Object, 0)
	for _, id := range rootIDs {
		if obj, ok := ch.Heap.Objects[id]; ok {
			obj.Color = Grey
			greySet = append(greySet, obj)
		}
	}
	ch.mu.Unlock()
	timings.STWMarkSetup = time.Since(stwStart)
	// ── END STW #1 — mutators resume ─────────────────────────────

	// ── CONCURRENT MARK ──────────────────────────────────────────
	// Process grey objects one at a time, releasing the lock between
	// each to let mutators interleave. When a mutator's write barrier
	// shades an object grey, it pushes it directly into ch.GreyQueue.
	// Each iteration we drain that queue into our local work list —
	// no full-heap scan needed.
	markStart := time.Now()
	for {
		ch.mu.Lock()

		// Drain barrier-greyed objects from the shared queue into
		// our local work list. This is O(k) where k is the number
		// of barrier fires since the last drain — not O(n) over
		// the entire heap.
		for _, obj := range ch.DrainGrey() {
			if obj.Color == Grey {
				greySet = append(greySet, obj)
			}
		}

		if len(greySet) == 0 {
			ch.mu.Unlock()
			break
		}

		current := greySet[0]
		greySet = greySet[1:]

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

		// Yield to let mutators run — simulates the GC sharing CPU
		// with the application, just like real Go's concurrent mark.
		time.Sleep(time.Microsecond)
	}
	timings.ConcurrentMark = time.Since(markStart)
	// ── END CONCURRENT MARK ──────────────────────────────────────

	// ── STW PAUSE #2: Mark termination + Sweep ───────────────────
	// All mutators are blocked again. In real Go, this pause:
	//   - confirms no grey objects remain (mark termination)
	//   - disables the write barrier
	//   - initiates sweep (real Go sweeps concurrently too, but
	//     for clarity we sweep under the lock here — Phase 7 will
	//     make this concurrent)
	stwStart = time.Now()
	ch.mu.Lock()
	ch.Heap.Marking = false
	ch.Heap.GreyPusher = nil
	ch.GreyQueue = nil
	for id, obj := range ch.Heap.Objects {
		if obj.Color == White {
			delete(ch.Heap.Objects, id)
		}
	}
	ch.mu.Unlock()
	timings.STWSweep = time.Since(stwStart)
	// ── END STW #2 — mutators resume ─────────────────────────────

	return timings
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

// MutatorWithMetrics is Mutator with allocation tracking for Phase 5.
func MutatorWithMetrics(ch *ConcurrentHeap, rootIDs []int, done <-chan struct{}, wg *sync.WaitGroup, m *Metrics) {
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
			m.RecordAllocation()
			ids := heapIDs(ch.Heap)
			if len(ids) > 1 {
				parentID := ids[rng.Intn(len(ids))]
				parent := ch.Heap.Objects[parentID]
				newChildren := make([]*Object, len(parent.Children)+1)
				copy(newChildren, parent.Children)
				newChildren[len(parent.Children)] = obj
				ReplaceChildren(ch.Heap, parent, newChildren)
			}
		}

		ids := heapIDs(ch.Heap)
		if len(ids) >= 2 {
			srcID := ids[rng.Intn(len(ids))]
			dstID := ids[rng.Intn(len(ids))]
			src := ch.Heap.Objects[srcID]
			dst := ch.Heap.Objects[dstID]
			if src != dst {
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
