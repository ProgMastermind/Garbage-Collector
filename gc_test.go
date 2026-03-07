package gc

import (
	"errors"
	"testing"
)

// helper: allocate n objects, return them as a slice.
func allocN(t *testing.T, heap *Heap, n int) []*Object {
	t.Helper()
	objs := make([]*Object, n)
	for i := range n {
		obj, err := heap.Alloc()
		if err != nil {
			t.Fatalf("unexpected alloc error on object %d: %v", i, err)
		}
		objs[i] = obj
	}
	return objs
}

// 1. Reachable objects survive collection.
func TestReachableObjectsSurvive(t *testing.T) {
	heap := NewHeap(10)
	objs := allocN(t, heap, 3)

	// Build a chain: root → A → B → C
	objs[0].Children = []*Object{objs[1]}
	objs[1].Children = []*Object{objs[2]}
	roots := []int{objs[0].ID}

	MarkSweep(heap, roots)

	for _, obj := range objs {
		if _, ok := heap.Objects[obj.ID]; !ok {
			t.Errorf("reachable object %d was collected", obj.ID)
		}
	}
}

// 2. Unreachable objects are collected.
func TestUnreachableObjectsCollected(t *testing.T) {
	heap := NewHeap(10)
	objs := allocN(t, heap, 4)

	// Root points to A only; B and C are unreachable, D is reachable from A.
	root, a, b, c := objs[0], objs[1], objs[2], objs[3]
	root.Children = []*Object{a}

	roots := []int{root.ID}
	MarkSweep(heap, roots)

	// root and a survive
	for _, obj := range []*Object{root, a} {
		if _, ok := heap.Objects[obj.ID]; !ok {
			t.Errorf("reachable object %d was collected", obj.ID)
		}
	}
	// b and c are swept
	for _, obj := range []*Object{b, c} {
		if _, ok := heap.Objects[obj.ID]; ok {
			t.Errorf("unreachable object %d survived collection", obj.ID)
		}
	}
}

// 3. A cycle of unreachable objects is collected (A→B→C→A).
func TestUnreachableCycleCollected(t *testing.T) {
	heap := NewHeap(10)
	objs := allocN(t, heap, 4)

	root := objs[0]
	a, b, c := objs[1], objs[2], objs[3]

	// Unreachable cycle: A→B→C→A
	a.Children = []*Object{b}
	b.Children = []*Object{c}
	c.Children = []*Object{a}

	// Root does NOT point to any of {A, B, C}
	roots := []int{root.ID}
	MarkSweep(heap, roots)

	if _, ok := heap.Objects[root.ID]; !ok {
		t.Fatal("root was collected")
	}
	for _, obj := range []*Object{a, b, c} {
		if _, ok := heap.Objects[obj.ID]; ok {
			t.Errorf("unreachable cycle member %d survived collection", obj.ID)
		}
	}
}

// 4. After marking completes, zero grey objects remain.
func TestNoGreyAfterMarking(t *testing.T) {
	heap := NewHeap(10)
	objs := allocN(t, heap, 5)

	// Build a small graph: 0→1→2, 0→3, 4 is garbage.
	objs[0].Children = []*Object{objs[1], objs[3]}
	objs[1].Children = []*Object{objs[2]}

	roots := []int{objs[0].ID}
	MarkSweep(heap, roots)

	for _, obj := range heap.Objects {
		if obj.Color == Grey {
			t.Errorf("object %d is still grey after collection", obj.ID)
		}
	}
}

// 5. Allocating beyond heap capacity returns an error.
func TestHeapFullError(t *testing.T) {
	heap := NewHeap(3)
	allocN(t, heap, 3) // fill it up

	_, err := heap.Alloc()
	if err == nil {
		t.Fatal("expected error when heap is full, got nil")
	}
	if !errors.Is(err, ErrHeapFull) {
		t.Fatalf("expected ErrHeapFull, got: %v", err)
	}
}
