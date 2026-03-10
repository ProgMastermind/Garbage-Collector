package gc

import (
	"fmt"
	"sync"
	"testing"
)

// --- Phase 2 test (preserved): should still FAIL ---

// TestConcurrentMutatorBreaksGC demonstrates that concurrent mutation
// WITHOUT a write barrier violates the tri-color invariant.
// This test SHOULD FAIL.
func TestConcurrentMutatorBreaksGC(t *testing.T) {
	const (
		heapSize    = 50
		numMutators = 4
		iterations  = 500
		numRoots    = 3
	)

	for iter := range iterations {
		ch := NewConcurrentHeap(heapSize)

		rootIDs := make([]int, numRoots)
		ch.mu.Lock()
		for i := range numRoots {
			obj, err := ch.Heap.Alloc()
			if err != nil {
				t.Fatalf("failed to allocate root: %v", err)
			}
			rootIDs[i] = obj.ID
		}
		for range 10 {
			obj, _ := ch.Heap.Alloc()
			if obj != nil {
				root := ch.Heap.Objects[rootIDs[iter%numRoots]]
				root.Children = append(root.Children, obj)
			}
		}
		ch.mu.Unlock()

		done := make(chan struct{})
		var wg sync.WaitGroup
		for range numMutators {
			wg.Add(1)
			go MutatorUnsafe(ch, rootIDs, done, &wg) // no barrier
		}

		ConcurrentMarkSweepUnsafe(ch, rootIDs) // no barrier

		close(done)
		wg.Wait()

		ch.mu.Lock()
		reachable := walkRoots(ch.Heap, rootIDs)
		for obj := range reachable {
			if _, alive := ch.Heap.Objects[obj.ID]; !alive {
				chain := findPathFromRoots(ch.Heap, rootIDs, obj)
				ch.mu.Unlock()
				t.Fatalf(
					"ITERATION %d: reachable object %d was incorrectly swept!\n"+
						"  Path from root: %s\n"+
						"  This is the tri-color invariant violation.",
					iter, obj.ID, formatPath(chain),
				)
			}
		}
		ch.mu.Unlock()
	}
}

// --- Phase 3 tests ---

// TestWriteBarrierFixesGC is the fixed version of TestConcurrentMutatorBreaksGC.
// With the hybrid write barrier enabled, no reachable object should be swept.
func TestWriteBarrierFixesGC(t *testing.T) {
	const (
		heapSize    = 50
		numMutators = 4
		iterations  = 500
		numRoots    = 3
	)

	for iter := range iterations {
		ch := NewConcurrentHeap(heapSize)

		rootIDs := make([]int, numRoots)
		ch.mu.Lock()
		for i := range numRoots {
			obj, err := ch.Heap.Alloc()
			if err != nil {
				t.Fatalf("failed to allocate root: %v", err)
			}
			rootIDs[i] = obj.ID
		}
		for range 10 {
			obj, _ := ch.Heap.Alloc()
			if obj != nil {
				root := ch.Heap.Objects[rootIDs[iter%numRoots]]
				ReplaceChildren(ch.Heap, root, append(root.Children, obj))
			}
		}
		ch.mu.Unlock()

		done := make(chan struct{})
		var wg sync.WaitGroup
		for range numMutators {
			wg.Add(1)
			go Mutator(ch, rootIDs, done, &wg) // with barrier
		}

		ConcurrentMarkSweep(ch, rootIDs) // with barrier

		close(done)
		wg.Wait()

		ch.mu.Lock()
		reachable := walkRoots(ch.Heap, rootIDs)
		for obj := range reachable {
			if _, alive := ch.Heap.Objects[obj.ID]; !alive {
				chain := findPathFromRoots(ch.Heap, rootIDs, obj)
				ch.mu.Unlock()
				t.Fatalf(
					"ITERATION %d: reachable object %d was incorrectly swept!\n"+
						"  Path from root: %s\n"+
						"  Write barrier should have prevented this.",
					iter, obj.ID, formatPath(chain),
				)
			}
		}
		ch.mu.Unlock()
	}
}

// TestWriteBarrierShadesTargets verifies that the write barrier shades
// both the old and new targets grey when a black object's pointer is
// rewritten.
func TestWriteBarrierShadesTargets(t *testing.T) {
	h := NewHeap(10)

	// Allocate before marking starts so objects are born white.
	src, _ := h.Alloc()
	oldChild, _ := h.Alloc()
	newChild, _ := h.Alloc()

	// Now enable marking. src is black (already scanned),
	// both children are white (not yet discovered).
	h.Marking = true
	src.Color = Black
	src.Children = []*Object{oldChild}

	// Rewrite src's pointer from oldChild → newChild.
	ReplaceChildren(h, src, []*Object{newChild})

	if oldChild.Color != Grey {
		t.Errorf("old target should be grey, got %s", oldChild.Color)
	}
	if newChild.Color != Grey {
		t.Errorf("new target should be grey, got %s", newChild.Color)
	}
}

// TestBarrierNoOpOutsideMarking verifies the barrier does nothing when
// the mark phase flag is off.
func TestBarrierNoOpOutsideMarking(t *testing.T) {
	h := NewHeap(10)
	h.Marking = false // barrier inactive

	src, _ := h.Alloc()
	oldChild, _ := h.Alloc()
	newChild, _ := h.Alloc()

	src.Color = Black
	src.Children = []*Object{oldChild}

	ReplaceChildren(h, src, []*Object{newChild})

	if oldChild.Color != White {
		t.Errorf("old target should remain white (barrier off), got %s", oldChild.Color)
	}
	if newChild.Color != White {
		t.Errorf("new target should remain white (barrier off), got %s", newChild.Color)
	}
}

// TestStressConcurrentGCWithBarrier runs 4 mutators + concurrent GC for
// 1000 cycles and verifies no reachable object is ever incorrectly swept.
func TestStressConcurrentGCWithBarrier(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	const (
		heapSize    = 80
		numMutators = 4
		iterations  = 1000
		numRoots    = 3
	)

	for iter := range iterations {
		ch := NewConcurrentHeap(heapSize)

		rootIDs := make([]int, numRoots)
		ch.mu.Lock()
		for i := range numRoots {
			obj, err := ch.Heap.Alloc()
			if err != nil {
				t.Fatalf("failed to allocate root: %v", err)
			}
			rootIDs[i] = obj.ID
		}
		for range 15 {
			obj, _ := ch.Heap.Alloc()
			if obj != nil {
				root := ch.Heap.Objects[rootIDs[iter%numRoots]]
				ReplaceChildren(ch.Heap, root, append(root.Children, obj))
			}
		}
		ch.mu.Unlock()

		done := make(chan struct{})
		var wg sync.WaitGroup
		for range numMutators {
			wg.Add(1)
			go Mutator(ch, rootIDs, done, &wg)
		}

		ConcurrentMarkSweep(ch, rootIDs)

		close(done)
		wg.Wait()

		ch.mu.Lock()
		reachable := walkRoots(ch.Heap, rootIDs)
		for obj := range reachable {
			if _, alive := ch.Heap.Objects[obj.ID]; !alive {
				chain := findPathFromRoots(ch.Heap, rootIDs, obj)
				ch.mu.Unlock()
				t.Fatalf(
					"ITERATION %d: reachable object %d was incorrectly swept!\n"+
						"  Path from root: %s",
					iter, obj.ID, formatPath(chain),
				)
			}
		}
		ch.mu.Unlock()
	}
}

// --- Phase 8 tests ---

// TestStackScanningDiscoversRoots verifies that the GC discovers root
// objects by scanning goroutine stacks rather than using hardcoded IDs.
func TestStackScanningDiscoversRoots(t *testing.T) {
	ch := NewConcurrentHeap(20)

	// Allocate objects and build a graph: root → child → grandchild.
	ch.mu.Lock()
	root, _ := ch.Heap.Alloc()
	child, _ := ch.Heap.Alloc()
	grandchild, _ := ch.Heap.Alloc()
	garbage1, _ := ch.Heap.Alloc()
	garbage2, _ := ch.Heap.Alloc()

	root.Children = []*Object{child}
	child.Children = []*Object{grandchild}
	// garbage1 and garbage2 have no references from any stack.

	// Register a goroutine stack that holds only the root object.
	stack := ch.RegisterStack()
	stack.Locals = []*Object{root}
	ch.mu.Unlock()

	// Run GC — it should scan the stack, find root, trace to child
	// and grandchild, and sweep garbage1 and garbage2.
	ConcurrentMarkSweep(ch, nil) // pass nil rootIDs — stacks are the roots now

	ch.mu.Lock()
	defer ch.mu.Unlock()

	// root, child, grandchild must survive.
	for _, obj := range []*Object{root, child, grandchild} {
		if _, ok := ch.Heap.Objects[obj.ID]; !ok {
			t.Errorf("reachable object %d (reachable via stack) was collected", obj.ID)
		}
	}
	// garbage1 and garbage2 must be collected.
	for _, obj := range []*Object{garbage1, garbage2} {
		if _, ok := ch.Heap.Objects[obj.ID]; ok {
			t.Errorf("unreachable object %d survived collection", obj.ID)
		}
	}
}

// TestMultipleStacksMultipleRoots verifies that objects held on different
// goroutine stacks are all treated as roots.
func TestMultipleStacksMultipleRoots(t *testing.T) {
	ch := NewConcurrentHeap(20)

	ch.mu.Lock()
	objA, _ := ch.Heap.Alloc()
	objB, _ := ch.Heap.Alloc()
	childOfA, _ := ch.Heap.Alloc()
	childOfB, _ := ch.Heap.Alloc()
	garbage, _ := ch.Heap.Alloc()

	objA.Children = []*Object{childOfA}
	objB.Children = []*Object{childOfB}

	// Two goroutines, each holding different roots.
	stack1 := ch.RegisterStack()
	stack1.Locals = []*Object{objA}
	stack2 := ch.RegisterStack()
	stack2.Locals = []*Object{objB}
	ch.mu.Unlock()

	ConcurrentMarkSweep(ch, nil)

	ch.mu.Lock()
	defer ch.mu.Unlock()

	// Everything reachable from either stack must survive.
	for _, obj := range []*Object{objA, objB, childOfA, childOfB} {
		if _, ok := ch.Heap.Objects[obj.ID]; !ok {
			t.Errorf("object %d reachable from a goroutine stack was collected", obj.ID)
		}
	}
	if _, ok := ch.Heap.Objects[garbage.ID]; ok {
		t.Errorf("garbage object %d survived collection", garbage.ID)
	}
}

// TestStackChurnDuringGC runs mutators that actively modify their stacks
// while GC runs concurrently, verifying no reachable object is lost.
func TestStackChurnDuringGC(t *testing.T) {
	const (
		heapSize    = 80
		numMutators = 4
		iterations  = 200
	)
	rootIDs := []int{0, 1, 2}

	for iter := range iterations {
		ch := NewConcurrentHeap(heapSize)

		ch.mu.Lock()
		for _, id := range rootIDs {
			obj := &Object{ID: id, Color: White}
			ch.Heap.Objects[id] = obj
			if id >= ch.Heap.nextID {
				ch.Heap.nextID = id + 1
			}
		}
		// Seed some children.
		for range 10 {
			obj, _ := ch.Heap.Alloc()
			if obj != nil {
				root := ch.Heap.Objects[rootIDs[iter%len(rootIDs)]]
				ReplaceChildren(ch.Heap, root, append(root.Children, obj))
			}
		}
		ch.mu.Unlock()

		done := make(chan struct{})
		var wg sync.WaitGroup
		for range numMutators {
			wg.Add(1)
			go Mutator(ch, rootIDs, done, &wg) // registers stack internally
		}

		ConcurrentMarkSweep(ch, rootIDs)

		close(done)
		wg.Wait()

		ch.mu.Lock()
		reachable := walkRoots(ch.Heap, rootIDs)
		for obj := range reachable {
			if _, alive := ch.Heap.Objects[obj.ID]; !alive {
				chain := findPathFromRoots(ch.Heap, rootIDs, obj)
				ch.mu.Unlock()
				t.Fatalf(
					"ITERATION %d: reachable object %d swept with stack scanning!\n"+
						"  Path: %s",
					iter, obj.ID, formatPath(chain),
				)
			}
		}
		ch.mu.Unlock()
	}
}

// --- Phase 9 tests ---

// TestGCAssistForcesMarkingWork verifies that a mutator with assist
// debt is forced to mark grey objects before it can proceed.
func TestGCAssistForcesMarkingWork(t *testing.T) {
	ch := NewConcurrentHeap(30)

	ch.mu.Lock()
	// Create some objects and make them grey (as if marking is in progress).
	root, _ := ch.Heap.Alloc()
	child1, _ := ch.Heap.Alloc()
	child2, _ := ch.Heap.Alloc()
	child3, _ := ch.Heap.Alloc()

	root.Children = []*Object{child1, child2, child3}

	// Simulate marking in progress: root is grey, children are white.
	ch.Heap.Marking = true
	ch.Heap.GreyPusher = func(obj *Object) { ch.PushGrey(obj) }
	root.Color = Grey
	ch.PushGrey(root)

	// Register a goroutine stack with assist debt of 2.
	stack := ch.RegisterStack()
	stack.Locals = []*Object{root}
	stack.AssistDebt = 2

	// Call GCAssist — it should mark objects to pay off the debt.
	marked := ch.GCAssist(stack)
	ch.mu.Unlock()

	if marked < 1 {
		t.Errorf("GCAssist should have marked at least 1 object, marked %d", marked)
	}
	if stack.AssistDebt != 0 {
		t.Errorf("assist debt should be 0 after assist, got %d", stack.AssistDebt)
	}
	// root should have been marked black (it was the grey object).
	if root.Color != Black {
		t.Errorf("root should be black after assist marked it, got %s", root.Color)
	}
}

// TestGCAssistNoDebtNoWork verifies that GCAssist is a no-op when
// the goroutine has no assist debt.
func TestGCAssistNoDebtNoWork(t *testing.T) {
	ch := NewConcurrentHeap(10)

	ch.mu.Lock()
	obj, _ := ch.Heap.Alloc()
	obj.Color = Grey
	ch.Heap.Marking = true

	stack := ch.RegisterStack()
	stack.AssistDebt = 0 // no debt

	marked := ch.GCAssist(stack)
	ch.mu.Unlock()

	if marked != 0 {
		t.Errorf("GCAssist with no debt should mark 0, marked %d", marked)
	}
}

// TestGCAssistClearsDebtWhenNoGreyLeft verifies that if there are no
// grey objects remaining, assist clears the debt (nothing to help with).
func TestGCAssistClearsDebtWhenNoGreyLeft(t *testing.T) {
	ch := NewConcurrentHeap(10)

	ch.mu.Lock()
	obj, _ := ch.Heap.Alloc()
	obj.Color = Black // no grey objects anywhere
	ch.Heap.Marking = true

	stack := ch.RegisterStack()
	stack.AssistDebt = 5

	ch.GCAssist(stack)
	ch.mu.Unlock()

	if stack.AssistDebt != 0 {
		t.Errorf("debt should be cleared when no grey objects exist, got %d", stack.AssistDebt)
	}
}

// TestGCAssistIntegration runs a full concurrent GC with mutators and
// verifies that assists occur during marking.
func TestGCAssistIntegration(t *testing.T) {
	ch := NewConcurrentHeap(100)
	metrics := NewMetrics(100)
	rootIDs := []int{0, 1, 2}

	ch.mu.Lock()
	for _, id := range rootIDs {
		obj := &Object{ID: id, Color: White}
		ch.Heap.Objects[id] = obj
		if id >= ch.Heap.nextID {
			ch.Heap.nextID = id + 1
		}
	}
	// Seed enough objects to create grey work.
	for range 20 {
		obj, _ := ch.Heap.Alloc()
		if obj != nil {
			root := ch.Heap.Objects[rootIDs[0]]
			root.Children = append(root.Children, obj)
		}
	}
	ch.mu.Unlock()

	done := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go MutatorWithMetrics(ch, rootIDs, done, &wg, metrics)
	}

	// Run GC — mutators should experience assists during mark phase.
	ConcurrentMarkSweep(ch, rootIDs)

	close(done)
	wg.Wait()

	// We can't guarantee assists will happen every time (depends on
	// timing), but verify the system doesn't crash and metrics work.
	t.Logf("Assist events: %d, objects marked by mutators: %d",
		metrics.totalAssists, metrics.totalAssistWork)
}

// --- Helpers (shared across Phase 2 and Phase 3 tests) ---

func walkRoots(h *Heap, rootIDs []int) map[*Object]bool {
	visited := make(map[*Object]bool)
	var stack []*Object

	for _, id := range rootIDs {
		if obj, ok := h.Objects[id]; ok {
			stack = append(stack, obj)
		}
	}
	for len(stack) > 0 {
		obj := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[obj] {
			continue
		}
		visited[obj] = true
		for _, child := range obj.Children {
			if !visited[child] {
				stack = append(stack, child)
			}
		}
	}
	return visited
}

func findPathFromRoots(h *Heap, rootIDs []int, target *Object) []*Object {
	type entry struct {
		obj  *Object
		path []*Object
	}
	visited := make(map[*Object]bool)
	var queue []entry

	for _, id := range rootIDs {
		if obj, ok := h.Objects[id]; ok {
			queue = append(queue, entry{obj, []*Object{obj}})
		}
	}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if visited[e.obj] {
			continue
		}
		visited[e.obj] = true
		for _, child := range e.obj.Children {
			newPath := make([]*Object, len(e.path)+1)
			copy(newPath, e.path)
			newPath[len(e.path)] = child
			if child == target {
				return newPath
			}
			if !visited[child] {
				queue = append(queue, entry{child, newPath})
			}
		}
	}
	return []*Object{target}
}

func formatPath(chain []*Object) string {
	s := ""
	for i, obj := range chain {
		if i > 0 {
			s += " → "
		}
		alive := "SWEPT"
		if obj.Color == Black {
			alive = "black"
		} else if obj.Color == Grey {
			alive = "grey"
		} else {
			alive = fmt.Sprintf("white/%s", "SWEPT")
		}
		s += fmt.Sprintf("[id=%d %s]", obj.ID, alive)
	}
	return s
}
