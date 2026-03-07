package gc

import (
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentMutatorBreaksGC demonstrates that concurrent mutation
// without a write barrier violates the tri-color invariant: the GC
// can sweep a reachable object because a mutator rewired a pointer
// from a black object to a white object after the GC finished scanning
// the black object.
//
// This test SHOULD FAIL.
func TestConcurrentMutatorBreaksGC(t *testing.T) {
	const (
		heapSize   = 50
		numMutators = 4
		iterations = 500
		numRoots   = 3
	)

	for iter := range iterations {
		ch := NewConcurrentHeap(heapSize)

		// Create root objects.
		rootIDs := make([]int, numRoots)
		ch.mu.Lock()
		for i := range numRoots {
			obj, err := ch.Heap.Alloc()
			if err != nil {
				t.Fatalf("failed to allocate root: %v", err)
			}
			rootIDs[i] = obj.ID
		}
		// Pre-populate some objects so mutators have things to rewire.
		for range 10 {
			obj, _ := ch.Heap.Alloc()
			if obj != nil {
				// Attach to a random root so they start reachable.
				root := ch.Heap.Objects[rootIDs[iter%numRoots]]
				root.Children = append(root.Children, obj)
			}
		}
		ch.mu.Unlock()

		// Start mutators.
		done := make(chan struct{})
		var wg sync.WaitGroup
		for range numMutators {
			wg.Add(1)
			go Mutator(ch, rootIDs, done, &wg)
		}

		// Run concurrent GC.
		ConcurrentMarkSweep(ch, rootIDs)

		// Stop mutators.
		close(done)
		wg.Wait()

		// --- Verification ---
		// Walk the object graph from roots. Every reachable object must
		// still exist in the heap. If one is missing, the GC incorrectly
		// swept a live object.
		ch.mu.Lock()
		reachable := walkRoots(ch.Heap, rootIDs)
		for obj := range reachable {
			if _, alive := ch.Heap.Objects[obj.ID]; !alive {
				chain := findPathFromRoots(ch.Heap, rootIDs, obj)
				ch.mu.Unlock()
				t.Fatalf(
					"ITERATION %d: reachable object %d was incorrectly swept!\n"+
						"  Path from root: %s\n"+
						"  This is the tri-color invariant violation: a black object\n"+
						"  acquired a pointer to this white object after the GC\n"+
						"  finished scanning it, so the GC never discovered it.",
					iter, obj.ID, formatPath(chain),
				)
			}
		}
		ch.mu.Unlock()
	}
}

// walkRoots returns all objects reachable from rootIDs by following Children pointers.
// It works with stale pointers: if a child is in the set, we record it even if the
// heap no longer contains it (that's exactly the bug we want to detect).
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

// findPathFromRoots attempts to find a pointer chain from any root to target.
// Returns the chain as a slice of objects (root first, target last).
// Because the target has been swept, we follow stale pointers.
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
	return []*Object{target} // fallback: no path found
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
