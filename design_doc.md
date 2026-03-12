# Go Garbage Collector Simulation — Design Document

A progressive, phase-by-phase simulation of Go's tri-color concurrent mark-and-sweep garbage collector, built entirely in Go's standard library. Each phase adds one concept, building from a naive stop-the-world collector to a near-production-faithful concurrent GC with stack scanning, GC assist, and span-based memory.

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Phase 1: Heap Model and Stop-the-World Mark-Sweep](#phase-1-heap-model-and-stop-the-world-mark-sweep)
3. [Phase 2: Concurrent Mutators (Without Write Barrier)](#phase-2-concurrent-mutators-without-write-barrier)
4. [Phase 3: Hybrid Write Barrier](#phase-3-hybrid-write-barrier)
5. [Phase 4: GOGC-Based GC Pacing](#phase-4-gogc-based-gc-pacing)
6. [Phase 5: Runtime Metrics and Summary Report](#phase-5-runtime-metrics-and-summary-report)
7. [Phase 6: Shared Grey Work Queue and Allocate Black](#phase-6-shared-grey-work-queue-and-allocate-black)
8. [Phase 7: Concurrent Sweep](#phase-7-concurrent-sweep)
9. [Phase 8: Goroutine Stack Scanning](#phase-8-goroutine-stack-scanning)
10. [Phase 9: GC Assist](#phase-9-gc-assist)
11. [Phase 10: Span Memory Model with Per-Span Locking](#phase-10-span-memory-model-with-per-span-locking)
12. [Final Architecture](#final-architecture)
13. [How Our Simulation Maps to Real Go](#how-our-simulation-maps-to-real-go)

## Architecture Overview

The simulation is organized into these files:

```
gc.go              — Core types: Object, Span, Heap, Color, WriteBarrier, MarkSweep (STW)
concurrent.go      — ConcurrentHeap, ConcurrentMarkSweep, Mutators, GoroutineStack, GCAssist
pacer.go           — Pacer (GOGC trigger), RunWithPacer, GCStats
metrics.go         — Thread-safe Metrics, Summary report
gc_test.go         — Phase 1 tests (STW correctness)
concurrent_test.go — Phase 2-10 tests (concurrency, barrier, stacks, spans)
pacer_test.go      — Phase 4-5 tests (pacing, metrics)
cmd/demo/main.go   — Entry point: GOGC=10 vs GOGC=1000 comparison
```

The core data flow at completion:

```
Mutator goroutines                         GC goroutine
  |                                           |
  |-- Alloc() → span lock → fill slot        |
  |-- ReplaceChildren() → WriteBarrier()      |
  |-- stack.Locals push/pop                   |
  |                                           |
  |   ┌─── STW #1 ───────────────────────┐   |
  |   | Reset colors to white            |   |
  |   | Enable write barrier             |   |
  |   | Scan all goroutine stacks        |   |
  |   | Shade root objects grey          |   |
  |   └──────────────────────────────────┘   |
  |              (mutators resume)            |
  |                                           |
  |-- GCAssist: mark grey objects if in debt  |
  |-- WriteBarrier: shade old+new grey ───────|──→ GreyQueue
  |                                           |
  |                               DrainGrey() ←── GreyQueue
  |                               Mark grey→black
  |                               Shade children grey
  |                                           |
  |   ┌─── STW #2 ───────────────────────┐   |
  |   | Disable write barrier             |   |
  |   | Reset assist debt                 |   |
  |   | Build sweep queue (list of spans) |   |
  |   └──────────────────────────────────┘   |
  |              (mutators resume)            |
  |                                           |
  |                               For each span:
  |                                 span.mu.Lock()
  |                                 Free white slots
  |                                 span.mu.Unlock()
  |                                 (mutators can use other spans)
  |                                 ch.mu.Lock()
  |                                 delete from Objects index
  |                                 ch.mu.Unlock()
```


## Phase 1: Heap Model and Stop-the-World Mark-Sweep

**Goal:** Build the foundational types and a correct (but naive) garbage collector.

**What real Go has:** A tri-color mark-sweep algorithm as the core GC strategy.

**What we built:**

### The tri-color abstraction

Every heap object is in exactly one of three states:

```
White  — Not yet visited. Candidate for collection.
Grey   — Discovered but not fully scanned. Its children haven't been checked yet.
Black  — Fully scanned. This object and all its direct children are known to be reachable.
```

The invariant: when marking finishes, every reachable object is black and every white object is garbage.

### Core types

```go
type Object struct {
    ID       int
    Color    Color
    Children []*Object   // pointers to other heap objects
}

type Heap struct {
    MaxSize  int
    Objects  map[int]*Object   // all live objects, indexed by ID
    nextID   int
}
```

The `Children` slice represents the pointer graph — object A pointing to object B means A has B in its `Children`. This is separate from the `Objects` map, which is the heap's bookkeeping of what exists.

### The algorithm

```go
func MarkSweep(heap *Heap, rootIDs []int) {
    // 1. Reset: everything starts white
    for _, obj := range heap.Objects {
        obj.Color = White
    }

    // 2. Shade roots grey (these are directly reachable)
    greySet := []*Object{}
    for _, id := range rootIDs {
        obj := heap.Objects[id]
        obj.Color = Grey
        greySet = append(greySet, obj)
    }

    // 3. Process grey objects until none remain
    for len(greySet) > 0 {
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

    // 4. Sweep: delete everything still white
    for id, obj := range heap.Objects {
        if obj.Color == White {
            delete(heap.Objects, id)
        }
    }
}
```

**Why a loop instead of recursion?** The grey set is a work queue. Recursion would blow the stack on deep object graphs. The loop processes grey objects BFS-style — predictable memory usage, easy to reason about.

**Design choice: `map[int]*Object`** — We chose a map for the heap because it gives O(1) lookup by ID, O(1) deletion, and easy iteration. Real Go uses spans and bitmaps, but for learning the GC algorithm, a map is the clearest representation.

### State after Phase 1

```
┌──────────────────────────────────┐
│ Heap (map[int]*Object)           │
│                                  │
│ MarkSweep: stop the world,      │
│   mark reachable, sweep white    │
│                                  │
│ No concurrency. No mutators.     │
│ Correct but impractical.         │
└──────────────────────────────────┘
```


## Phase 2: Concurrent Mutators (Without Write Barrier)

**Goal:** Add goroutines that allocate and rewire pointers concurrently with the GC — and watch the GC break.

**What real Go has:** Application goroutines run during GC. Without protection, this causes the "lost object" problem.

**What we built:**

### ConcurrentHeap

```go
type ConcurrentHeap struct {
    mu   sync.Mutex
    Heap *Heap
}
```

A wrapper with a mutex. The GC and mutators share the heap, locking/unlocking around each operation.

### Mutators

Each mutator goroutine runs in a loop:
1. Lock the heap
2. Allocate a new object, attach it as a child of a random existing object
3. Rewire: pick a random object, point it at another random object
4. Unlock
5. Sleep briefly (simulate real work)

### The bug: tri-color invariant violation

The GC marks objects one at a time, releasing the lock between each. A mutator can:

1. GC scans object A (now black), sees child B (shaded grey)
2. Mutator runs: moves B's pointer from A to C (C is already black)
3. GC processes B, finds nothing new
4. Object that B pointed to (D) is still white — but D is reachable through C→...→D
5. GC sweeps D. **But D was reachable.**

This is the tri-color invariant violation: a black object (C) points to a white object (D) with no grey object in between.

```
Before mutator runs:           After mutator runs:
  A(black) → B(grey) → D        A(black)    B(grey) → D
                                 C(black) → D

  GC will find D via B ✓         GC won't rescan C (it's black)
                                 D stays white → COLLECTED ✗
```

### The intentional test failure

`TestConcurrentMutatorBreaksGC` runs this scenario 500 times. It uses `ConcurrentMarkSweepUnsafe` (no write barrier) and `MutatorUnsafe` (direct pointer writes). It SHOULD fail — that's the point. Seeing the failure makes Phase 3's fix meaningful.

### State after Phase 2

```
┌──────────────────────────────────┐
│ ConcurrentHeap (Heap + Mutex)    │
│                                  │
│ GC + Mutators run concurrently   │
│ GC releases lock between objects │
│ Mutators rewire pointers freely  │
│                                  │
│ BUG: reachable objects collected │
│      (tri-color violation)       │
└──────────────────────────────────┘
```


## Phase 3: Hybrid Write Barrier

**Goal:** Fix the tri-color violation by adding Go's hybrid write barrier.

**What real Go has:** A hybrid barrier (since Go 1.8) that shades both the old and new targets of every pointer write during the mark phase.

**What we built:**

### The write barrier

```go
func WriteBarrier(h *Heap, old, new_ *Object) {
    if !h.Marking { return }          // no-op outside mark phase

    // Deletion barrier: shade the old target
    if old != nil && old.Color == White {
        old.Color = Grey
    }
    // Insertion barrier: shade the new target
    if new_ != nil && new_.Color == White {
        new_.Color = Grey
    }
}
```

**Why shade both?**

- **Insertion barrier (shade new):** If a black object gains a pointer to a white object, shade the white object grey so the GC will discover it. This fixes: "black points to white with no grey in between."

- **Deletion barrier (shade old):** If a grey object loses its pointer to a white object, shade the old target grey so the GC doesn't lose track of it. This fixes: "the only path to a white object was through a grey object, and that path just got deleted."

Neither barrier alone is sufficient:

- Insertion-only: If object A (grey) drops its pointer to D (white), and B (black) picks it up — the insertion barrier shades D when B gets it. But if D was only reachable through A, and A's pointer was the one removed, D could be lost before B's write happens.

- Deletion-only: If B (black) gains a pointer to D (white) by a completely new allocation path (not by moving an existing pointer), the deletion barrier never fires because nothing was deleted.

The hybrid barrier closes both holes.

### Safe pointer operations

```go
// Safe single-child update
func SetChild(h *Heap, src *Object, idx int, new_ *Object) {
    old := src.Children[idx]
    WriteBarrier(h, old, new_)
    src.Children[idx] = new_
}

// Safe full replacement
func ReplaceChildren(h *Heap, src *Object, newChildren []*Object) {
    for _, old := range src.Children {
        WriteBarrier(h, old, nil)       // shade every removed child
    }
    for _, new_ := range newChildren {
        WriteBarrier(h, nil, new_)      // shade every added child
    }
    src.Children = newChildren
}
```

The barrier fires BEFORE the actual pointer write. This is critical — if the write happened first, there's a window where the invariant is broken.

### How it fixes the Phase 2 bug

```
A(black) → B(grey) → D(white)

Mutator: ReplaceChildren(A, [C])  // remove B, add C
  Barrier fires:
    WriteBarrier(h, B, nil)   → B was grey, no-op (only shades white)
    WriteBarrier(h, nil, C)   → C is black, no-op

Mutator: ReplaceChildren(B, [])   // remove D from B
  Barrier fires:
    WriteBarrier(h, D, nil)   → D is white → shade D GREY ✓

D is now grey. The GC will discover and scan it. No lost objects.
```

### ConcurrentMarkSweep (3-phase structure)

The GC now runs in three phases:

```
STW #1:  Lock → reset colors to white → enable barrier (Marking=true) → shade roots grey → Unlock

         Concurrent Mark:  Lock one object → mark it → unlock → yield → repeat

STW #2:  Lock → disable barrier (Marking=false) → sweep all white objects → Unlock
```

Mutators run freely during the concurrent mark phase. The write barrier ensures they can't break the tri-color invariant.

### Go 1.7 vs Go 1.8 barrier history

Go 1.7 used a **Dijkstra insertion barrier only** — it shaded the new target but not the old. This meant goroutine stacks needed special treatment: stacks didn't have write barriers (too expensive for every local variable write), so the GC had to **rescan all stacks** during STW #2 to catch any pointers that were moved from the stack to the heap during marking. This made STW #2 slow (proportional to total stack size).

Go 1.8 switched to the **hybrid barrier** — shade both old and new. This eliminated the need to rescan stacks, making STW #2 tiny (just disable the barrier and sweep).

Our barrier matches Go 1.8's hybrid design.

### State after Phase 3

```
┌──────────────────────────────────────────┐
│ ConcurrentHeap + Hybrid Write Barrier    │
│                                          │
│ STW #1 → Concurrent Mark → STW #2       │
│                                          │
│ Barrier shades old+new grey on every     │
│ pointer write during marking.            │
│                                          │
│ Tri-color invariant maintained. ✓        │
│ No reachable objects collected.  ✓       │
└──────────────────────────────────────────┘
```


## Phase 4: GOGC-Based GC Pacing

**Goal:** Instead of triggering GC manually, trigger it automatically when the heap grows enough — just like real Go.

**What real Go has:** The GOGC environment variable controls how much the heap can grow before the next GC. `GOGC=100` (default) means "collect when the heap doubles since the last collection."

**What we built:**

### The Pacer

```go
type Pacer struct {
    GOGC            int
    liveAfterLastGC int   // surviving objects from the most recent GC
}

func (p *Pacer) ShouldCollect(heapSize int) bool {
    goal := float64(p.liveAfterLastGC) * (1.0 + float64(p.GOGC)/100.0)
    return float64(heapSize) >= goal
}
```

The formula: `trigger when heapSize >= liveAfterLastGC * (1 + GOGC/100)`

- `GOGC=100`: trigger when heap doubles (100% growth allowed)
- `GOGC=10`: trigger at 10% growth (very aggressive, frequent GC)
- `GOGC=1000`: trigger at 1000% growth (very lazy, rare GC)
- `GOGC=0`: trigger constantly (collect after every allocation batch)

### RunWithPacer

The main loop that ties everything together:

```
┌─────────────────────────────────────────────────┐
│ Pre-create root objects                         │
│ Start N mutator goroutines                      │
│                                                 │
│ Loop until deadline:                            │
│   1. Read heap size                             │
│   2. Ask pacer: ShouldCollect?                  │
│   3. If yes: run ConcurrentMarkSweep            │
│   4. Record stats, update pacer                 │
│   5. Sleep briefly                              │
│                                                 │
│ Stop mutators, return stats                     │
└─────────────────────────────────────────────────┘
```

### The timing problem (caught and fixed)

Initially, we measured "STW pause" as the entire `ConcurrentMarkSweep` call duration. This was wrong — STW only covers the brief periods where the lock is held for setup and teardown. The concurrent mark phase (where mutators run freely) is NOT STW time.

Fix: `ConcurrentMarkSweep` returns `GCPhaseTimings` with separate measurements:

```go
type GCPhaseTimings struct {
    STWMarkSetup   time.Duration   // actual STW #1 pause
    ConcurrentMark time.Duration   // NOT STW — mutators run
    STWMarkTermination time.Duration // actual STW #2 pause
}
```

### Effect of GOGC on behavior

```
GOGC=10:   Frequent collection → low peak heap → more CPU on GC
GOGC=1000: Rare collection → high peak heap → less CPU on GC
```

This is the fundamental trade-off in garbage collection: memory vs CPU.

### State after Phase 4

```
┌───────────────────────────────────────────┐
│ Pacer: triggers GC at heap growth target  │
│                                           │
│ GOGC controls the trade-off:              │
│   Low GOGC  → frequent GC, low memory    │
│   High GOGC → rare GC, high memory       │
│                                           │
│ Per-phase timing: true STW vs concurrent  │
└───────────────────────────────────────────┘
```


## Phase 5: Runtime Metrics and Summary Report

**Goal:** Track GC performance across all cycles and produce a human-readable summary.

**What real Go has:** `GODEBUG=gctrace=1` prints per-cycle GC stats. The `runtime/metrics` package exposes detailed GC data.

**What we built:**

### Thread-safe Metrics

```go
type Metrics struct {
    mu sync.Mutex

    gcCycles      int
    totalSTW1     time.Duration
    totalSTW2     time.Duration
    totalMarkTime time.Duration
    totalGCTime   time.Duration
    minSTW        time.Duration
    maxSTW        time.Duration
    peakHeapSize  int

    totalAllocated atomic.Int64   // bumped by mutators (no lock needed)
    totalCollected atomic.Int64
}
```

Allocation counting uses `atomic.Int64` so mutators don't need to acquire the mutex just to bump a counter. Everything else is updated by the GC goroutine under the mutex.

### Summary report

```
────────────────────────────────────────────────────────────
  GC Simulation Summary
────────────────────────────────────────────────────────────

  Configuration
    GOGC            : 100
    Heap capacity   : 500 objects
    Mutators        : 2

  Collection
    GC cycles       : 15
    Total GC time   : 85ms
    Avg cycle time  : 5.6ms

  Stop-the-World Pauses
    Total STW time  : 450µs
    Avg STW pause   : 15µs
    Min STW pause   : 2µs
    Max STW pause   : 45µs

  Concurrent Work
    Mark time       : 84ms
    Concurrent %    : 99.5% of total GC time
```

### MutatorWithMetrics

A variant of `Mutator` that calls `metrics.RecordAllocation()` on every successful alloc. The pacer loop calls `metrics.RecordGCCycle(stats)` after each collection.

### State after Phase 5

```
┌───────────────────────────────────────────┐
│ Full metrics pipeline:                    │
│                                           │
│ Mutators → RecordAllocation (atomic)      │
│ GC       → RecordGCCycle (mutex)          │
│ Pacer    → ObserveHeapSize                │
│                                           │
│ Summary(): formatted report with all      │
│ timing, heap, and object statistics       │
└───────────────────────────────────────────┘
```


## Phase 6: Shared Grey Work Queue and Allocate Black

**Goal:** Eliminate the O(n) full-heap scan for finding grey objects. Make the write barrier feed directly into the GC's work list.

**What real Go has:** Per-P `gcWork` queues. When the write barrier shades an object grey, it pushes it onto the local P's work queue. The GC pops from these queues.

### The problem before Phase 6

During concurrent mark, the GC found barrier-greyed objects by scanning the entire heap every iteration:

```go
for _, obj := range ch.Heap.Objects {
    if obj.Color == Grey {
        greySet = append(greySet, obj)
    }
}
```

With 10,000 objects, that's 10,000 checks per iteration — just to find the 1-2 objects the barrier greyed. O(n) per barrier fire.

### The fix: GreyPusher callback

The `Heap` gets a function pointer that the write barrier calls when it greys an object:

```go
type Heap struct {
    ...
    GreyPusher func(obj *Object)
}
```

During STW #1, we wire it up:

```go
ch.Heap.GreyPusher = func(obj *Object) {
    ch.PushGrey(obj)   // O(1) — append to GreyQueue slice
}
```

Now the write barrier pushes directly to the queue:

```go
func WriteBarrier(h *Heap, old, new_ *Object) {
    if new_ != nil && new_.Color == White {
        new_.Color = Grey
        if h.GreyPusher != nil {
            h.GreyPusher(new_)   // → straight into the work queue
        }
    }
}
```

The GC drains the queue each iteration:

```go
for _, obj := range ch.DrainGrey() {
    if obj.Color == Grey {
        greySet = append(greySet, obj)
    }
}
```

`DrainGrey` is O(k) where k is the number of barrier fires since the last drain — not O(n) over the heap.

During STW #2, we unplug the callback:

```go
ch.Heap.GreyPusher = nil   // marking is over, stop sending
```

### Why GreyPusher is a callback

`gc.go` (low-level) doesn't know about `ConcurrentHeap` or `GreyQueue` (high-level). The callback lets the high-level layer inject behavior into the low-level barrier without import cycles.

### Allocate black during marking

```go
func (h *Heap) Alloc() (*Object, error) {
    color := White
    if h.Marking {
        color = Black   // born black: no children to scan
    }
    ...
}
```

New objects born during marking start black. They have no children yet, so there's nothing to scan. This avoids the barrier overhead of discovering them later.

### State after Phase 6

```
┌──────────────────────────────────────────────────┐
│ Write Barrier → GreyPusher → GreyQueue → GC     │
│                                                  │
│ O(1) per barrier fire instead of O(n) heap scan  │
│ New objects born black during marking             │
│                                                  │
│ ConcurrentHeap:                                  │
│   GreyQueue []*Object                            │
│   PushGrey(obj)  — barrier calls this            │
│   DrainGrey()    — GC calls this each iteration  │
└──────────────────────────────────────────────────┘
```


## Phase 7: Concurrent Sweep

**Goal:** Move the sweep phase out of STW into concurrent execution, reducing pause times.

**What real Go has:** Sweeping happens concurrently — each goroutine sweeps a span before allocating from it.

### The problem before Phase 7

STW #2 held the mutex for the entire sweep:

```go
ch.mu.Lock()
ch.Heap.Marking = false
// Delete ALL white objects while mutators are blocked
for id, obj := range ch.Heap.Objects {
    if obj.Color == White {
        delete(ch.Heap.Objects, id)
    }
}
ch.mu.Unlock()   // mutators finally resume
```

If there were 200 white objects, mutators froze for all 200 deletions.

### The fix: split STW #2 into mark termination + concurrent sweep

**STW #2 (mark termination)** — tiny, just bookkeeping:
```go
ch.mu.Lock()
ch.Heap.Marking = false
ch.Heap.GreyPusher = nil
// Build the sweep queue — just collect IDs, don't delete anything
for id, obj := range ch.Heap.Objects {
    if obj.Color == White {
        ch.Heap.SweepQueue = append(ch.Heap.SweepQueue, id)
    }
}
ch.Heap.Sweeping = true
ch.mu.Unlock()   // mutators resume IMMEDIATELY
```

**Concurrent sweep** — batched, lock released between batches:
```go
const sweepBatchSize = 16
for {
    ch.mu.Lock()
    if len(ch.Heap.SweepQueue) == 0 {
        ch.Heap.Sweeping = false
        ch.mu.Unlock()
        break
    }
    batch := ch.Heap.SweepQueue[:16]
    ch.Heap.SweepQueue = ch.Heap.SweepQueue[16:]
    for _, id := range batch {
        delete(ch.Heap.Objects, id)
    }
    ch.mu.Unlock()   // mutators can interleave between batches
}
```

### Four-phase GC structure

The GC now has the same four phases as real Go:

```
STW #1 (mark setup)        →  reset colors, enable barrier, shade roots
Concurrent Mark             →  trace grey objects, mutators run freely
STW #2 (mark termination)  →  disable barrier, build sweep queue (tiny!)
Concurrent Sweep            →  delete white objects in batches
```

### Why this is safe

The sweep queue was built during STW #2 when no mutators were running. Every white object was confirmed unreachable at that moment. New allocations during the sweep start white (marking is off), but they're not in the queue — the queue was frozen at mark termination.

### State after Phase 7

```
┌──────────────────────────────────────────────────┐
│ Four-phase GC:                                   │
│                                                  │
│   STW #1: mark setup (brief)                     │
│   Concurrent mark (mutators run)                 │
│   STW #2: mark termination (tiny — no deletion)  │
│   Concurrent sweep (mutators interleave)          │
│                                                  │
│ STW #2 went from O(garbage) to O(heap-read-only) │
└──────────────────────────────────────────────────┘
```


## Phase 8: Goroutine Stack Scanning

**Goal:** Replace hardcoded root IDs with dynamic root discovery by scanning goroutine stacks — exactly how real Go finds roots.

**What real Go has:** During STW #1, the runtime pauses every goroutine and scans its stack frame by frame, looking for pointers into the heap. These pointers become roots.

### The problem before Phase 8

Roots were hardcoded:

```go
rootIDs := []int{0, 1, 2}   // never changes, decided before the program starts
```

Every GC cycle started from the same three objects. This is unrealistic — in a real program:
- Goroutines come and go
- Functions call and return, changing local variables
- The set of "what's on the stack right now" changes constantly

### GoroutineStack

Each mutator goroutine gets a simulated stack:

```go
type GoroutineStack struct {
    ID     int
    Locals []*Object   // simulated local variables pointing into the heap
}
```

**Lifecycle:**
1. Mutator starts → `ch.RegisterStack()` → seeds stack with initial root objects
2. Mutator allocates → `stack.Locals = append(stack.Locals, obj)` (local variable)
3. Mutator drops locals → `stack.Locals = stack.Locals[1:]` (function returned)
4. Mutator picks up references → `stack.Locals = append(stack.Locals, existingObj)`
5. Mutator exits → `ch.UnregisterStack(stack)`

### Stack scanning during STW #1

```go
func (ch *ConcurrentHeap) ScanStacks() []*Object {
    seen := make(map[int]bool)
    var roots []*Object
    for _, gs := range ch.Stacks {
        for _, obj := range gs.Locals {
            if obj != nil && !seen[obj.ID] {
                if _, alive := ch.Heap.Objects[obj.ID]; alive {
                    seen[obj.ID] = true
                    roots = append(roots, obj)
                }
            }
        }
    }
    return roots
}
```

During STW #1, if stacks are registered, the GC scans them instead of using hardcoded rootIDs:

```go
if len(ch.Stacks) > 0 {
    stackRoots := ch.ScanStacks()
    for _, obj := range stackRoots {
        obj.Color = Grey
        greySet = append(greySet, obj)
    }
} else {
    // Legacy path for older tests
    for _, id := range rootIDs { ... }
}
```

### Stack churn

Mutators simulate realistic stack frame changes:

```go
// Function returned — drop oldest local variable
if len(stack.Locals) > 5 && rng.Intn(3) == 0 {
    stack.Locals = stack.Locals[1:]
}
// New local variable — pick up reference to existing object
if len(ids) > 0 && rng.Intn(4) == 0 {
    randomID := ids[rng.Intn(len(ids))]
    stack.Locals = append(stack.Locals, ch.Heap.Objects[randomID])
}
```

This means the root set is different every GC cycle:

```
Cycle 1 roots: {obj0, obj1, obj2, obj10, obj11}
Cycle 2 roots: {obj2, obj10, obj50, obj77}
Cycle 3 roots: {obj50, obj77, obj103}
```

Objects that were roots in cycle 1 might be garbage in cycle 3 if no goroutine holds them anymore.

### State after Phase 8

```
┌──────────────────────────────────────────────────┐
│ Goroutine Stacks:                                │
│                                                  │
│   Each mutator: GoroutineStack with Locals       │
│   Register on start, unregister on exit          │
│   Push/pop locals as functions call/return       │
│                                                  │
│ STW #1: ScanStacks() → dynamic root discovery    │
│   Roots change between cycles                    │
│   Objects only on stack are still found           │
└──────────────────────────────────────────────────┘
```


## Phase 9: GC Assist

**Goal:** When mutators allocate faster than the GC can mark, force them to help — creating backpressure.

**What real Go has:** `gcAssistAlloc` — when a goroutine allocates, if it has negative GC credit, it must do marking work proportional to the allocation before proceeding.

### The problem before Phase 9

Mutators and the GC were independent:

```
Mutator:   alloc → alloc → alloc → alloc → alloc (doesn't care about GC)
GC:        mark..........mark..........mark....... (falling behind)
```

If mutators allocate faster than the GC can mark, the heap fills up before marking finishes. Without assist, the only option is a longer STW pause.

### Assist debt

Each `GoroutineStack` tracks debt:

```go
type GoroutineStack struct {
    ID         int
    Locals     []*Object
    AssistDebt int   // allocations during marking that haven't been "paid for"
}
```

When a mutator allocates during marking, it incurs one unit of debt:

```go
if ch.Heap.Marking {
    stack.AssistDebt++
}
```

Before the next allocation, it must pay off the debt:

```go
ch.GCAssist(stack)   // called at the start of each mutator iteration
```

### GCAssist: real marking work

```go
func (ch *ConcurrentHeap) GCAssist(gs *GoroutineStack) int {
    if !ch.Heap.Marking || gs.AssistDebt <= 0 {
        return 0
    }

    marked := 0
    for gs.AssistDebt > 0 {
        // Find a grey object (from barrier queue or heap scan)
        target := findGreyObject()

        if target == nil {
            gs.AssistDebt = 0   // nothing to help with
            break
        }

        // Do actual marking work
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
```

The mutator isn't pretending — it's doing **real** marking. It grabs a grey object, traces its children, marks it black. That object won't be processed again by the GC.

### Debt reset at mark termination

```go
// In STW #2
for _, gs := range ch.Stacks {
    gs.AssistDebt = 0   // marking is over, debt doesn't carry forward
}
```

### How real Go differs

Real Go's assist is more sophisticated:
- **Dynamic ratio:** `(remaining mark work) / (remaining heap budget)`. Near the end of marking, the ratio drops. Near the heap goal, it spikes.
- **Global credit pool:** If one goroutine does extra marking, other goroutines can steal that credit.
- Our version uses a simple 1:1 ratio — same concept, simpler math.

### State after Phase 9

```
┌──────────────────────────────────────────────────┐
│ GC Assist:                                       │
│                                                  │
│ Mutator allocates during marking → AssistDebt++  │
│ Before next alloc → GCAssist: mark grey objects  │
│ 1 allocation = 1 grey object marked              │
│                                                  │
│ Effect: fast allocators help GC keep up           │
│ Backpressure prevents heap from running away     │
└──────────────────────────────────────────────────┘
```


## Phase 10: Span Memory Model with Per-Span Locking

**Goal:** Replace the flat `map[int]*Object` with structured spans, each with its own mutex — enabling true per-span concurrency.

**What real Go has:** `mspan` — contiguous blocks of memory holding same-sized objects. Each span has its own lock. The GC sweeps one span at a time while goroutines allocate from other spans.

### The problem before Phase 10

One global mutex protected everything:

```
┌─────────────────────────────────────────────────┐
│  Heap.Objects = map[int]*Object                 │
│                                                 │
│  Protected by: ch.mu (one lock for everything)  │
│                                                 │
│  GC sweeps obj0 → mutator B waits              │
│  GC sweeps obj1 → mutator B still waits        │
│  GC sweeps obj2 → mutator B still waits        │
│  ...                                            │
│  GC done → mutator B finally runs              │
└─────────────────────────────────────────────────┘
```

### Span type

```go
const SpanSize = 8   // 8 object slots per span

type Span struct {
    mu    sync.Mutex           // per-span lock
    ID    int
    Slots [SpanSize]*Object    // nil slots are free
    Count int                  // number of occupied slots
}
```

### Object back-pointers

Each object knows where it lives:

```go
type Object struct {
    ID       int
    Color    Color
    Children []*Object
    span     *Span     // "I live in this span"
    slotIdx  int       // "I'm at slot index 3"
}
```

### Heap with spans

```go
type Heap struct {
    MaxSize    int
    Objects    map[int]*Object   // global index (kept for fast lookups)
    Spans      []*Span           // the actual storage
    ...
}
```

We keep the `Objects` map as a global index for ID-based lookups (used by tests and mutators). The spans are the actual storage with per-span locking.

### Allocation into spans

```go
func (h *Heap) Alloc() (*Object, error) {
    ...
    span := h.findFreeSpan()    // find or create a span with room
    span.mu.Lock()
    span.allocSlot(obj)         // place obj in a free slot
    span.mu.Unlock()

    h.Objects[obj.ID] = obj     // update global index
    return obj, nil
}
```

`findFreeSpan` locks each span briefly to check if it's full:

```go
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
    // All full — create a new span
    s := &Span{ID: h.nextSpanID}
    h.nextSpanID++
    h.Spans = append(h.Spans, s)
    return s
}
```

### Per-span concurrent sweep

This is the key improvement. Before: one lock for all deletions. After:

```go
for {
    // Pop one span from the queue (brief global lock)
    ch.mu.Lock()
    if len(ch.Heap.SweepQueue) == 0 {
        ch.Heap.Sweeping = false
        ch.mu.Unlock()
        break
    }
    span := ch.Heap.SweepQueue[0]
    ch.Heap.SweepQueue = ch.Heap.SweepQueue[1:]
    ch.mu.Unlock()

    // Lock ONLY this span — mutators can use other spans freely
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

    // Brief global lock to update the Objects index
    if len(deadIDs) > 0 {
        ch.mu.Lock()
        for _, id := range deadIDs {
            delete(ch.Heap.Objects, id)
        }
        ch.mu.Unlock()
    }
}
```

While Span 0 is being swept, mutators can allocate from Span 1, 2, 3, etc. The sweep of one span doesn't block operations on any other span.

### Build sweep queue from spans

STW #2 now builds a span-based sweep queue:

```go
ch.Heap.SweepQueue = ch.Heap.SweepQueue[:0]
for _, span := range ch.Heap.Spans {
    for _, obj := range span.Slots {
        if obj != nil && obj.Color == White {
            ch.Heap.SweepQueue = append(ch.Heap.SweepQueue, span)
            break   // one white object is enough to enqueue the span
        }
    }
}
```

### The remaining global lock

We still briefly touch `ch.mu` to update the `Objects` map after sweeping a span. This is because Go maps aren't concurrency-safe. In real Go, there's no global object map — everything works through direct pointers and span-level bitmaps, so sweeping is purely span-local.

### State after Phase 10

```
┌──────────────────────────────────────────────────────────────┐
│ Span Memory Model:                                           │
│                                                              │
│ ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐        │
│ │ Span 0  │  │ Span 1  │  │ Span 2  │  │ Span 3  │  ...   │
│ │ mu      │  │ mu      │  │ mu      │  │ mu      │        │
│ │ 8 slots │  │ 8 slots │  │ 8 slots │  │ 8 slots │        │
│ └─────────┘  └─────────┘  └─────────┘  └─────────┘        │
│                                                              │
│ Alloc: lock span → fill slot → unlock                        │
│ Sweep: lock span → free whites → unlock → next span          │
│        (mutators use other spans during sweep)               │
│                                                              │
│ Objects map kept as global index for ID lookups              │
│ Objects know their span via back-pointer (obj.span)          │
└──────────────────────────────────────────────────────────────┘
```


## Final Architecture

After all 10 phases, the simulation models:

```
┌────────────────────────────────────────────────────────────────────┐
│                        GC Simulation                               │
│                                                                    │
│  Mutator Goroutines (N)              GC Goroutine                  │
│  ┌──────────────────┐               ┌──────────────────┐          │
│  │ GoroutineStack   │               │ ConcurrentMarkSweep          │
│  │   .Locals        │               │                              │
│  │   .AssistDebt    │               │ STW #1: ScanStacks()         │
│  │                  │               │   reset colors               │
│  │ Loop:            │               │   enable barrier             │
│  │  GCAssist()      │               │   shade roots grey           │
│  │  Alloc → span    │               │                              │
│  │  ReplaceChildren │──barrier──────│→ GreyQueue                   │
│  │  stack churn     │               │                              │
│  └──────────────────┘               │ Concurrent Mark:             │
│                                     │   DrainGrey → mark → yield   │
│  Pacer                              │                              │
│  ┌──────────────────┐               │ STW #2: build sweep queue    │
│  │ GOGC threshold   │               │   reset assist debt          │
│  │ ShouldCollect()  │               │                              │
│  │ RecordCycle()    │               │ Concurrent Sweep:            │
│  └──────────────────┘               │   per-span locking           │
│                                     └──────────────────┘          │
│  Metrics                                                           │
│  ┌──────────────────┐                                              │
│  │ GC cycles, STW   │   Spans                                     │
│  │ times, heap peak, │   ┌───┐┌───┐┌───┐┌───┐                    │
│  │ alloc/collected,  │   │ 0 ││ 1 ││ 2 ││ 3 │ ...                │
│  │ stacks scanned,  │   │mu ││mu ││mu ││mu │                    │
│  │ assists, spans    │   └───┘└───┘└───┘└───┘                    │
│  └──────────────────┘                                              │
└────────────────────────────────────────────────────────────────────┘
```


## How Our Simulation Maps to Real Go

| Concept | Real Go | Our Simulation | Fidelity |
|---------|---------|----------------|----------|
| Tri-color marking | White/grey/black via mark bits | White/grey/black Color field | Exact |
| Write barrier | Hybrid (shade old + new), Go 1.8+ | Hybrid (shade old + new) | Exact |
| Concurrent mark | GC goroutines trace in parallel | Single GC goroutine, lock-per-object | Semantic match |
| GC pacing | GOGC + soft memory limit | GOGC formula | Exact |
| Root discovery | Scan goroutine stacks word-by-word | ScanStacks() over Locals slice | Semantic match |
| GC assist | Dynamic ratio, per-P credit pool | 1:1 ratio, per-goroutine debt | Simplified |
| Memory spans | mspan with size classes, per-P mcache | Fixed-size Span with per-span mutex | Simplified |
| Concurrent sweep | Lazy sweep-before-alloc per span | Sweep all dirty spans in loop | Simplified |
| Born-black alloc | New objects black during mark | Same | Exact |
| Grey work queue | Per-P gcWork queues | Shared GreyQueue via GreyPusher | Simplified |
| Four-phase GC | STW1 → mark → STW2 → sweep | Same | Exact |
| STW pauses | <1ms typical | Microsecond range | Proportional |

### What we intentionally simplified

1. **Size classes** — Real spans hold same-sized objects (8B, 16B, 32B...). Ours treat all objects as the same size. This is an allocator optimization, not a GC semantic.

2. **Per-P mcache** — Real Go gives each processor a local span cache for lock-free allocation. Ours locks the span. This is a scheduler integration, not a GC semantic.

3. **Sweep-before-alloc** — Real Go sweeps lazily: "you want memory from this span? sweep it first." Ours sweeps all dirty spans after marking. Same result, different timing.

4. **Dynamic assist ratio** — Real Go calculates assist work based on remaining mark work vs remaining heap budget. Ours uses a flat 1:1 ratio. Same concept, less adaptive.

### What we got right

Every **semantic** aspect of Go's GC is modeled:
- The algorithm (tri-color mark-sweep)
- The concurrency strategy (concurrent mark + sweep with STW bookends)
- The correctness mechanism (hybrid write barrier)
- The scheduling policy (GOGC-based pacing)
- The root discovery mechanism (stack scanning)
- The backpressure mechanism (GC assist)
- The memory organization (spans with per-span locking)
- The optimization (born-black allocation, shared work queue)

The simulation is a faithful semantic model of Go's garbage collector, built progressively from first principles.
