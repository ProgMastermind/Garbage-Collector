package gc

import (
	"errors"
	"fmt"
)

// --- Tri-color abstraction ---

type Color int

const (
	White Color = iota // Unvisited — candidate for collection
	Grey               // Discovered — still needs its referents scanned
	Black              // Scanned — reachable and fully processed
)

func (c Color) String() string {
	switch c {
	case White:
		return "white"
	case Grey:
		return "grey"
	case Black:
		return "black"
	default:
		return "unknown"
	}
}

// --- Object ---

type Object struct {
	ID       int
	Color    Color
	Children []*Object // Pointers to other heap objects
}

func (o *Object) String() string {
	childIDs := make([]int, len(o.Children))
	for i, c := range o.Children {
		childIDs[i] = c.ID
	}
	return fmt.Sprintf("Object{id=%d, color=%s, children=%v}", o.ID, o.Color, childIDs)
}

// --- Heap ---

var ErrHeapFull = errors.New("heap is full: cannot allocate")

type Heap struct {
	MaxSize int
	Objects map[int]*Object // Live objects indexed by ID
	nextID  int
}

func NewHeap(maxSize int) *Heap {
	return &Heap{
		MaxSize: maxSize,
		Objects: make(map[int]*Object),
	}
}

// Alloc creates a new white object on the heap.
func (h *Heap) Alloc() (*Object, error) {
	if len(h.Objects) >= h.MaxSize {
		return nil, ErrHeapFull
	}
	obj := &Object{
		ID:    h.nextID,
		Color: White,
	}
	h.Objects[obj.ID] = obj
	h.nextID++
	return obj, nil
}

// --- Mark-Sweep Collector (stop-the-world) ---

// MarkSweep performs a full stop-the-world collection.
// rootIDs identifies the set of objects directly reachable from goroutine stacks.
func MarkSweep(heap *Heap, rootIDs []int) {
	// Phase 1: Reset — mark everything white.
	for _, obj := range heap.Objects {
		obj.Color = White
	}

	// Phase 2: Shade roots grey.
	greySet := make([]*Object, 0)
	for _, id := range rootIDs {
		if obj, ok := heap.Objects[id]; ok {
			obj.Color = Grey
			greySet = append(greySet, obj)
		}
	}

	// Phase 3: Process grey objects until none remain.
	//   For each grey object:
	//     - shade any white children grey (discover them)
	//     - shade the object itself black  (done scanning it)
	for len(greySet) > 0 {
		// Pop from the front (BFS-style, easy to follow).
		current := greySet[0]
		greySet = greySet[1:]

		for _, child := range current.Children {
			if child.Color == White {
				child.Color = Grey
				greySet = append(greySet, child)
			}
		}
		current.Color = Black
	}

	// Phase 4: Sweep — free every object still white (unreachable).
	for id, obj := range heap.Objects {
		if obj.Color == White {
			delete(heap.Objects, id)
		}
	}
}
