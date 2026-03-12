package gc

import (
	"math/rand"
	"sync"
	"time"
)

// GoroutineStack simulates a goroutine's stack frame. Each mutator
// maintains a set of local variable pointers; the GC scans all
// registered stacks during STW #1 to discover roots.
type GoroutineStack struct {
	ID         int       // goroutine identifier
	Locals     []*Object // simulated local variables pointing into the heap
	AssistDebt int       // objects allocated during marking that haven't been "paid for"
}

// ConcurrentHeap wraps a Heap with a mutex for safe concurrent access.
// GreyQueue is a shared work queue fed by the write barrier.
// Stacks holds registered goroutine stacks for root discovery.
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

// ScanStacks walks all registered stacks and returns every unique
// heap object found. Must be called while ch.mu is held.
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

// GCAssist forces a mutator to mark grey objects to pay off its assist
// debt (1 allocation = 1 grey object to mark). Must be called while
// ch.mu is held. Returns the number of objects marked.
func (ch *ConcurrentHeap) GCAssist(gs *GoroutineStack) int {
	if !ch.Heap.Marking || gs.AssistDebt <= 0 {
		return 0
	}

	marked := 0
	for gs.AssistDebt > 0 {
		drained := ch.DrainGrey()

		var target *Object
		for _, obj := range drained {
			if obj.Color == Grey {
				target = obj
				break
			}
		}

		if target == nil {
			for _, obj := range ch.Heap.Objects {
				if obj.Color == Grey {
					target = obj
					break
				}
			}
		}

		if target == nil {
			gs.AssistDebt = 0
			break
		}

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

// ConcurrentMarkSweep runs mark-sweep with the hybrid write barrier.
// Returns per-phase timings (STW #1 → concurrent mark → STW #2 → sweep).
func ConcurrentMarkSweep(ch *ConcurrentHeap, rootIDs []int) GCPhaseTimings {
	var timings GCPhaseTimings

	// STW #1: mark setup
	stwStart := time.Now()
	ch.mu.Lock()
	for _, obj := range ch.Heap.Objects {
		obj.Color = White
	}
	ch.Heap.Marking = true
	ch.GreyQueue = nil
	ch.Heap.GreyPusher = func(obj *Object) {
		ch.PushGrey(obj)
	}

	// Scan goroutine stacks to find root pointers. Fall back to
	// hardcoded rootIDs for backward compatibility with older tests.
	greySet := make([]*Object, 0)
	if len(ch.Stacks) > 0 {
		timings.StacksScanned = len(ch.Stacks)
		stackRoots := ch.ScanStacks()
		timings.RootsFromStacks = len(stackRoots)
		for _, obj := range stackRoots {
			obj.Color = Grey
			greySet = append(greySet, obj)
		}
	} else {
		for _, id := range rootIDs {
			if obj, ok := ch.Heap.Objects[id]; ok {
				obj.Color = Grey
				greySet = append(greySet, obj)
			}
		}
	}
	ch.mu.Unlock()
	timings.STWMarkSetup = time.Since(stwStart)

	// Concurrent mark: process grey objects one at a time, releasing
	// the lock between each to let mutators interleave.
	markStart := time.Now()
	for {
		ch.mu.Lock()

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

		time.Sleep(time.Microsecond)
	}
	timings.ConcurrentMark = time.Since(markStart)

	// STW #2: mark termination
	stwStart = time.Now()
	ch.mu.Lock()
	ch.Heap.Marking = false
	ch.Heap.GreyPusher = nil
	ch.GreyQueue = nil
	for _, gs := range ch.Stacks {
		gs.AssistDebt = 0
	}
	ch.Heap.SweepQueue = ch.Heap.SweepQueue[:0]
	for _, span := range ch.Heap.Spans {
		for _, obj := range span.Slots {
			if obj != nil && obj.Color == White {
				ch.Heap.SweepQueue = append(ch.Heap.SweepQueue, span)
				break
			}
		}
	}
	ch.Heap.Sweeping = true
	ch.mu.Unlock()
	timings.STWMarkTermination = time.Since(stwStart)

	// Concurrent sweep: one span at a time with per-span locking.
	sweepStart := time.Now()
	spansSwept := 0
	for {
		ch.mu.Lock()
		if len(ch.Heap.SweepQueue) == 0 {
			ch.Heap.Sweeping = false
			ch.mu.Unlock()
			break
		}
		span := ch.Heap.SweepQueue[0]
		ch.Heap.SweepQueue = ch.Heap.SweepQueue[1:]
		ch.mu.Unlock()

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
	return timings
}

// Mutator simulates a running goroutine that allocates, rewires pointers,
// and maintains a stack of local references for root discovery.
func Mutator(ch *ConcurrentHeap, rootIDs []int, done <-chan struct{}, wg *sync.WaitGroup) {
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

		ch.GCAssist(stack)

		obj, err := ch.Heap.Alloc()
		if err == nil {
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
