package gc

import (
	"errors"
	"fmt"
	"sync"
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
	span     *Span     // which span this object lives in
	slotIdx  int       // index within the span's Slots array
}

func (o *Object) String() string {
	childIDs := make([]int, len(o.Children))
	for i, c := range o.Children {
		childIDs[i] = c.ID
	}
	return fmt.Sprintf("Object{id=%d, color=%s, children=%v}", o.ID, o.Color, childIDs)
}

// --- Span ---

// SpanSize is the number of object slots per span. In real Go, spans
// hold objects of a specific size class (8B, 16B, 32B, etc.). Our
// simulation uses a fixed slot count since all objects are the same
// "size." Each span has its own mutex, enabling per-span locking
// during sweep and allocation — the key benefit over a single global lock.
const SpanSize = 8

// Span represents a contiguous block of memory holding up to SpanSize
// objects. In real Go, this is mspan — the fundamental unit of memory
// management. Each span has its own lock so the GC can sweep one span
// while mutators allocate from another, without either blocking.
type Span struct {
	mu    sync.Mutex
	ID    int
	Slots [SpanSize]*Object // nil slots are free
	Count int               // number of occupied slots
}

func (s *Span) isFull() bool {
	return s.Count >= SpanSize
}

// allocSlot places obj into the first free slot of this span.
// Sets the object's back-pointer to this span.
func (s *Span) allocSlot(obj *Object) {
	for i := range s.Slots {
		if s.Slots[i] == nil {
			s.Slots[i] = obj
			s.Count++
			obj.span = s
			obj.slotIdx = i
			return
		}
	}
}

// freeSlot removes the object at the given slot index.
func (s *Span) freeSlot(idx int) {
	s.Slots[idx] = nil
	s.Count--
}

// --- Heap ---

var ErrHeapFull = errors.New("heap is full: cannot allocate")

type Heap struct {
	MaxSize    int
	Objects    map[int]*Object   // Global index: object ID → object (for fast lookup)
	Spans      []*Span           // All spans in the heap
	nextID     int
	nextSpanID int
	Marking    bool              // True while the GC is in the mark phase
	Sweeping   bool              // True while concurrent sweep is in progress
	SweepQueue []*Span           // Spans containing white objects to sweep
	GreyPusher func(obj *Object) // If set, called to push grey objects to the shared work queue
}

func NewHeap(maxSize int) *Heap {
	return &Heap{
		MaxSize: maxSize,
		Objects: make(map[int]*Object),
	}
}

// findFreeSpan returns a span with at least one free slot,
// creating a new span if all existing spans are full.
// Locks each span briefly to safely read its count — this is
// necessary because the concurrent sweep modifies span counts
// under span locks, not the global lock.
func (h *Heap) findFreeSpan() *Span {
	for _, s := range h.Spans {
		s.mu.Lock()
		full := s.isFull()
		if !full {
			s.mu.Unlock()
			return s
		}
		s.mu.Unlock()
	}
	// All spans full — create a new one.
	s := &Span{ID: h.nextSpanID}
	h.nextSpanID++
	h.Spans = append(h.Spans, s)
	return s
}

// Insert places an existing object into the heap (Objects map + span).
// Used for manually creating root objects with specific IDs.
func (h *Heap) Insert(obj *Object) {
	h.Objects[obj.ID] = obj
	span := h.findFreeSpan()
	span.mu.Lock()
	span.allocSlot(obj)
	span.mu.Unlock()
	if obj.ID >= h.nextID {
		h.nextID = obj.ID + 1
	}
}

// Alloc creates a new object on the heap. During the mark phase, new
// objects are born black — they have no outgoing pointers yet, so
// marking them black is safe and avoids unnecessary barrier work.
// Outside the mark phase, objects start white (ready for next cycle).
//
// The object is placed into a span with a free slot. If no span has
// room, a new span is created automatically.
func (h *Heap) Alloc() (*Object, error) {
	if len(h.Objects) >= h.MaxSize {
		return nil, ErrHeapFull
	}
	color := White
	if h.Marking {
		color = Black // born black: no children to scan
	}
	obj := &Object{
		ID:    h.nextID,
		Color: color,
	}

	// Place the object into a span. Lock the span to safely
	// modify its slots — the concurrent sweep may be accessing
	// other spans simultaneously under their own locks.
	span := h.findFreeSpan()
	span.mu.Lock()
	span.allocSlot(obj)
	span.mu.Unlock()

	h.Objects[obj.ID] = obj
	h.nextID++
	return obj, nil
}

// --- Hybrid Write Barrier ---

// WriteBarrier is called on every pointer store during the mark phase.
// It implements Go's hybrid barrier: shade BOTH the old target and the
// new target grey. This closes two holes:
//   - Deletion barrier (shade old): if a grey object loses its pointer to
//     a white object, shading the old target ensures the GC still finds it.
//   - Insertion barrier (shade new): if a black object gains a pointer to
//     a white object, shading the new target ensures the GC still finds it.
//
// When the GC is not in the mark phase, this is a no-op.
//
// When GreyPusher is set (concurrent mode), greyed objects are pushed
// directly onto the shared work queue — O(1) per barrier fire.
// The GC drains this queue instead of scanning the entire heap.
func WriteBarrier(h *Heap, old, new_ *Object) {
	if !h.Marking {
		return
	}
	if old != nil && old.Color == White {
		old.Color = Grey
		if h.GreyPusher != nil {
			h.GreyPusher(old)
		}
	}
	if new_ != nil && new_.Color == White {
		new_.Color = Grey
		if h.GreyPusher != nil {
			h.GreyPusher(new_)
		}
	}
}

// SetChild is the safe way for mutators to update a pointer during GC.
// It fires the write barrier before performing the actual write.
//   src.Children[idx] = new_
func SetChild(h *Heap, src *Object, idx int, new_ *Object) {
	var old *Object
	if idx < len(src.Children) {
		old = src.Children[idx]
	}
	WriteBarrier(h, old, new_)
	if idx < len(src.Children) {
		src.Children[idx] = new_
	}
}

// ReplaceChildren is the safe way to overwrite an object's entire child
// list. Fires the barrier for every old child being removed and the new
// child being added.
func ReplaceChildren(h *Heap, src *Object, newChildren []*Object) {
	for _, old := range src.Children {
		WriteBarrier(h, old, nil)
	}
	for _, new_ := range newChildren {
		WriteBarrier(h, nil, new_)
	}
	src.Children = newChildren
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
	// Walk spans and free slots, then remove from the global index.
	for _, span := range heap.Spans {
		for i, obj := range span.Slots {
			if obj != nil && obj.Color == White {
				delete(heap.Objects, obj.ID)
				span.Slots[i] = nil
				span.Count--
			}
		}
	}
}
