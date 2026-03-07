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
	h.Marking = true // barrier active

	src, _ := h.Alloc()
	oldChild, _ := h.Alloc()
	newChild, _ := h.Alloc()

	// src is black, both children are white.
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
