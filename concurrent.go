package gc

import (
	"math/rand"
	"sync"
	"time"
)

// GoroutineStack simulates a goroutine's stack frame — the set of
// local variables (object pointers) that a goroutine currently holds.
// In real Go, the GC scans each goroutine's stack during STW #1 to
// discover root pointers. Our simulation mirrors this: each mutator
// maintains a stack of object references, and the GC scans all
// registered stacks to find roots.
//
// AssistDebt tracks how many objects this goroutine has allocated
// during the current mark phase without "paying" for them with
// marking work. When debt is positive, the mutator must mark grey
// objects before it is allowed to allocate — this is GC assist.
type GoroutineStack struct {
	ID         int       // goroutine identifier
	Locals     []*Object // simulated local variables pointing into the heap
	AssistDebt int       // objects allocated during marking that haven't been "paid for"
}

// ConcurrentHeap wraps a Heap with a mutex so mutators and the GC
// can share it safely. The GreyQueue is a shared work queue: the
// write barrier pushes grey objects here, and the GC pops from it.
// This replaces the O(n) full-heap scan from Phase 3.
//
// Stacks holds all registered goroutine stacks. During STW #1, the
// GC scans every stack to discover root pointers — just like real Go.
type ConcurrentHeap struct {
	mu        sync.Mutex
	Heap      *Heap
	GreyQueue []*Object          // shared work queue for barrier-greyed objects
	Stacks    map[int]*GoroutineStack // registered goroutine stacks (goroutine ID → stack)
	nextGID   int                     // next goroutine ID to assign
}

func NewConcurrentHeap(maxSize int) *ConcurrentHeap {
	return &ConcurrentHeap{
		Heap:   NewHeap(maxSize),
		Stacks: make(map[int]*GoroutineStack),
	}
}

// RegisterStack creates a new goroutine stack and returns it.
// The mutator owns the returned stack and updates its Locals as it runs.
// Must be called while ch.mu is held.
func (ch *ConcurrentHeap) RegisterStack() *GoroutineStack {
	gs := &GoroutineStack{ID: ch.nextGID}
	ch.Stacks[gs.ID] = gs
	ch.nextGID++
	return gs
}

// UnregisterStack removes a goroutine stack when the mutator exits.
// Must be called while ch.mu is held.
func (ch *ConcurrentHeap) UnregisterStack(gs *GoroutineStack) {
	delete(ch.Stacks, gs.ID)
}

// ScanStacks walks all registered goroutine stacks and returns every
// unique object found. This is the root discovery phase — equivalent
// to real Go's stack scanning during STW #1.
// Must be called while ch.mu is held.
func (ch *ConcurrentHeap) ScanStacks() []*Object {
	seen := make(map[int]bool)
	var roots []*Object
	for _, gs := range ch.Stacks {
		for _, obj := range gs.Locals {
			if obj != nil && !seen[obj.ID] {
				// Only include objects that still exist on the heap.
				if _, alive := ch.Heap.Objects[obj.ID]; alive {
					seen[obj.ID] = true
					roots = append(roots, obj)
				}
			}
		}
	}
	return roots
}

// GCAssist forces a mutator to do marking work to pay off its assist
// debt. For each object the mutator allocated during marking, it must
// mark one grey object black before it can allocate again.
//
// In real Go, the assist ratio is calculated dynamically based on
// remaining mark work vs remaining heap growth budget. Our version
// is simpler: 1 allocation = 1 grey object to mark. The effect is
// the same — fast allocators are forced to help the GC keep up.
//
// Must be called while ch.mu is held. Returns the number of objects
// the mutator marked (for metrics).
func (ch *ConcurrentHeap) GCAssist(gs *GoroutineStack) int {
	if !ch.Heap.Marking || gs.AssistDebt <= 0 {
		return 0
	}

	marked := 0
	for gs.AssistDebt > 0 {
		// First, drain any barrier-greyed objects from the shared queue.
		drained := ch.DrainGrey()

		// Find a grey object to mark.
		var target *Object
		for _, obj := range drained {
			if obj.Color == Grey {
				target = obj
				break
			}
			// Put non-grey objects back (they were already processed).
		}

		// If nothing from the drain, scan the heap for any remaining grey.
		if target == nil {
			for _, obj := range ch.Heap.Objects {
				if obj.Color == Grey {
					target = obj
					break
				}
			}
		}

		// No grey objects left — marking is essentially done.
		// Clear the debt; the mutator has nothing to help with.
		if target == nil {
			gs.AssistDebt = 0
			break
		}

		// Mark it: shade children grey, then shade target black.
		for _, child := range target.Children {
			if child.Color == White {
				child.Color = Grey
				ch.PushGrey(child)
			}
		}
		target.Color = Black
		marked++
		gs.AssistDebt--
	}

	return marked
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
	for _, span := range ch.Heap.Spans {
		for i, obj := range span.Slots {
			if obj != nil && obj.Color == White {
				delete(ch.Heap.Objects, obj.ID)
				span.Slots[i] = nil
				span.Count--
			}
		}
	}
	ch.mu.Unlock()
}

// --- Safe collector (with hybrid write barrier) ---

// GCPhaseTimings records how long each GC phase took.
type GCPhaseTimings struct {
	STWMarkSetup       time.Duration // STW pause #1: reset + enable barrier + shade roots
	ConcurrentMark     time.Duration // concurrent mark: trace objects (mutators run freely)
	STWMarkTermination time.Duration // STW pause #2: disable barrier, build sweep queue
	ConcurrentSweep    time.Duration // concurrent sweep: free white objects (mutators run freely)
	StacksScanned      int           // number of goroutine stacks scanned during STW #1
	RootsFromStacks    int           // number of root pointers found from stack scanning
	SpansSwept         int           // number of spans swept during concurrent sweep
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

	// Root discovery: scan all goroutine stacks to find root pointers.
	// If stacks are registered, they ARE the roots — just like real Go.
	// Fall back to rootIDs for backward compatibility with older tests.
	greySet := make([]*Object, 0)
	if len(ch.Stacks) > 0 {
		// Stack scanning: walk every goroutine's local variables.
		// Each pointer on the stack that points to a live heap object
		// becomes a root. This is exactly what real Go does during
		// STW #1 — it pauses goroutines and scans their stack frames
		// for pointers into the heap.
		timings.StacksScanned = len(ch.Stacks)
		stackRoots := ch.ScanStacks()
		timings.RootsFromStacks = len(stackRoots)
		for _, obj := range stackRoots {
			obj.Color = Grey
			greySet = append(greySet, obj)
		}
	} else {
		// Legacy path: use hardcoded root IDs (pre-Phase 8 tests).
		for _, id := range rootIDs {
			if obj, ok := ch.Heap.Objects[id]; ok {
				obj.Color = Grey
				greySet = append(greySet, obj)
			}
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

	// ── STW PAUSE #2: Mark termination ───────────────────────────
	// All mutators are blocked briefly. This pause ONLY:
	//   - disables the write barrier
	//   - builds the sweep queue (list of white object IDs)
	//   - sets the Sweeping flag
	// No objects are actually deleted here — that happens concurrently.
	// In real Go, this pause is typically <100μs.
	stwStart = time.Now()
	ch.mu.Lock()
	ch.Heap.Marking = false
	ch.Heap.GreyPusher = nil
	ch.GreyQueue = nil
	// Reset assist debt for all goroutines — marking is over,
	// so debt from this cycle no longer applies.
	for _, gs := range ch.Stacks {
		gs.AssistDebt = 0
	}
	// Build sweep queue: collect spans that contain white objects.
	// Unlike before (a flat list of object IDs), the sweep queue is
	// now a list of *spans*. This enables per-span locking during
	// the concurrent sweep — the GC locks one span at a time.
	ch.Heap.SweepQueue = ch.Heap.SweepQueue[:0]
	for _, span := range ch.Heap.Spans {
		for _, obj := range span.Slots {
			if obj != nil && obj.Color == White {
				ch.Heap.SweepQueue = append(ch.Heap.SweepQueue, span)
				break // one white object is enough to enqueue the span
			}
		}
	}
	ch.Heap.Sweeping = true
	ch.mu.Unlock()
	timings.STWMarkTermination = time.Since(stwStart)
	// ── END STW #2 — mutators resume ─────────────────────────────

	// ── CONCURRENT SWEEP (per-span locking) ─────────────────────
	// Sweep one span at a time. For each span, we lock ONLY that
	// span's mutex — not the global lock. This means mutators can
	// allocate from other spans while one span is being swept.
	//
	// In real Go, sweeping is even lazier: each goroutine sweeps a
	// span right before allocating from it. Our approach approximates
	// that by processing spans sequentially with per-span locks.
	sweepStart := time.Now()
	spansSwept := 0
	for {
		// Pop one span from the queue (brief global lock).
		ch.mu.Lock()
		if len(ch.Heap.SweepQueue) == 0 {
			ch.Heap.Sweeping = false
			ch.mu.Unlock()
			break
		}
		span := ch.Heap.SweepQueue[0]
		ch.Heap.SweepQueue = ch.Heap.SweepQueue[1:]
		ch.mu.Unlock()

		// Lock ONLY this span — mutators can work with other spans.
		span.mu.Lock()
		var deadIDs []int
		for i, obj := range span.Slots {
			if obj != nil && obj.Color == White {
				deadIDs = append(deadIDs, obj.ID)
				span.Slots[i] = nil
				span.Count--
			}
		}
		span.mu.Unlock()

		// Brief global lock to update the Objects index.
		if len(deadIDs) > 0 {
			ch.mu.Lock()
			for _, id := range deadIDs {
				delete(ch.Heap.Objects, id)
			}
			ch.mu.Unlock()
		}
		spansSwept++
	}
	timings.ConcurrentSweep = time.Since(sweepStart)
	timings.SpansSwept = spansSwept
	// ── END CONCURRENT SWEEP ─────────────────────────────────────

	return timings
}

// --- Mutator (with write barrier) ---

// Mutator simulates a running goroutine that continuously allocates and
// rewires pointers. Uses ReplaceChildren so the write barrier fires on
// every pointer store.
//
// Each mutator registers a goroutine stack with the ConcurrentHeap. As
// it allocates and works with objects, it maintains local references on
// its stack — just like a real goroutine holds pointers in local vars.
// During STW #1, the GC scans these stacks to discover root pointers.
func Mutator(ch *ConcurrentHeap, rootIDs []int, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Register this goroutine's stack. Seed it with the root objects
	// so the GC can find them via stack scanning.
	ch.mu.Lock()
	stack := ch.RegisterStack()
	for _, id := range rootIDs {
		if obj, ok := ch.Heap.Objects[id]; ok {
			stack.Locals = append(stack.Locals, obj)
		}
	}
	ch.mu.Unlock()

	defer func() {
		ch.mu.Lock()
		ch.UnregisterStack(stack)
		ch.mu.Unlock()
	}()

	for {
		select {
		case <-done:
			return
		default:
		}

		ch.mu.Lock()

		// GC assist: if this goroutine has assist debt from prior
		// allocations during the mark phase, it must do marking work
		// before it is allowed to allocate again. This creates
		// backpressure — fast allocators help the GC keep up.
		ch.GCAssist(stack)

		// Allocate a new object if there's room.
		obj, err := ch.Heap.Alloc()
		if err == nil {
			// The new object is a local variable on our stack.
			stack.Locals = append(stack.Locals, obj)

			// Incur assist debt: this allocation happened during marking,
			// so the mutator owes the GC one unit of marking work next time.
			if ch.Heap.Marking {
				stack.AssistDebt++
			}

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

		// Simulate stack frame churn: occasionally drop old locals and
		// pick up new references — models function returns and new
		// local variable assignments. This is critical because it means
		// roots change between GC cycles, just like in a real program.
		if len(stack.Locals) > 5 && rng.Intn(3) == 0 {
			// Drop the oldest local (function returned, var went out of scope).
			stack.Locals = stack.Locals[1:]
		}
		if len(ids) > 0 && rng.Intn(4) == 0 {
			// Pick up a reference to an existing object (new local variable).
			randomID := ids[rng.Intn(len(ids))]
			stack.Locals = append(stack.Locals, ch.Heap.Objects[randomID])
		}

		ch.mu.Unlock()
		time.Sleep(time.Duration(rng.Intn(500)) * time.Microsecond)
	}
}

// MutatorWithMetrics is Mutator with allocation tracking for Phase 5.
func MutatorWithMetrics(ch *ConcurrentHeap, rootIDs []int, done <-chan struct{}, wg *sync.WaitGroup, m *Metrics) {
	defer wg.Done()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	ch.mu.Lock()
	stack := ch.RegisterStack()
	for _, id := range rootIDs {
		if obj, ok := ch.Heap.Objects[id]; ok {
			stack.Locals = append(stack.Locals, obj)
		}
	}
	ch.mu.Unlock()

	defer func() {
		ch.mu.Lock()
		ch.UnregisterStack(stack)
		ch.mu.Unlock()
	}()

	for {
		select {
		case <-done:
			return
		default:
		}

		ch.mu.Lock()

		// GC assist: pay off any debt before allocating.
		assisted := ch.GCAssist(stack)
		if assisted > 0 {
			m.RecordAssist(assisted)
		}

		obj, err := ch.Heap.Alloc()
		if err == nil {
			m.RecordAllocation()
			stack.Locals = append(stack.Locals, obj)

			if ch.Heap.Marking {
				stack.AssistDebt++
			}

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

		// Stack frame churn: drop old locals, pick up new references.
		if len(stack.Locals) > 5 && rng.Intn(3) == 0 {
			stack.Locals = stack.Locals[1:]
		}
		if len(ids) > 0 && rng.Intn(4) == 0 {
			randomID := ids[rng.Intn(len(ids))]
			stack.Locals = append(stack.Locals, ch.Heap.Objects[randomID])
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
