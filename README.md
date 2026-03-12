# Go GC Simulation

A from-scratch simulation of Go's tri-color concurrent mark-and-sweep garbage collector, built progressively across 10 phases. Each phase introduces one real GC concept — starting from a naive stop-the-world collector and ending with concurrent mark-sweep, stack scanning, GC assist, and span-based memory with per-span locking.

The goal isn't performance. It's to understand how Go's GC actually works by building each piece yourself and watching it break (Phase 2) and then fix itself (Phase 3 onward).

Built with Go's standard library only. No external dependencies.

## Running

```bash
# Run the GOGC comparison demo (GOGC=10 vs GOGC=1000)
go run ./cmd/demo

# Run the pacer demo (per-cycle GC stats printed live)
go run ./cmd/pacer_demo

# Run all tests (the Phase 2 test fails intentionally — it proves the bug)
go test -race -count=1 ./...

# Run only the tests that should pass
go test -race -count=1 -run 'TestWriteBarrier|TestBarrier|TestStress|TestStack|TestGCAssist|TestSpan|TestPerSpan|TestSTWMark|TestInsert|TestMultiple|TestPacer|TestGOGC|TestMetrics|TestReachable|TestUnreachable|TestNoGrey|TestHeapFull' ./...
```

## Project Structure

```
gc.go              Core types (Object, Span, Heap, Color), WriteBarrier, MarkSweep (STW)
concurrent.go      ConcurrentHeap, ConcurrentMarkSweep, Mutators, GoroutineStack, GCAssist
pacer.go           Pacer (GOGC-based trigger), RunWithPacer, GCStats
metrics.go         Thread-safe runtime metrics and summary report
gc_test.go         STW correctness tests
concurrent_test.go Concurrency, barrier, stack scanning, assist, span tests
pacer_test.go      Pacing and metrics tests
cmd/demo/          GOGC comparison entry point
cmd/pacer_demo/    Per-cycle stats entry point
design_doc.md      Detailed phase-by-phase design document
```

## Phases

**Phase 1 — Stop-the-world mark-sweep.** Tri-color abstraction, heap as `map[int]*Object`, BFS grey queue. Correct but blocks everything.

**Phase 2 — Concurrent mutators (no barrier).** Goroutines allocate and rewire pointers while the GC runs. The tri-color invariant breaks — reachable objects get collected. `TestConcurrentMutatorBreaksGC` proves this by failing.

**Phase 3 — Hybrid write barrier.** Shade both old and new pointer targets grey on every write during marking. Matches Go 1.8+'s hybrid barrier. The invariant holds. No more lost objects.

**Phase 4 — GOGC-based pacing.** Trigger GC when `heapSize >= liveAfterLastGC * (1 + GOGC/100)`. Low GOGC = frequent GC, low memory. High GOGC = rare GC, high memory. Same formula as real Go.

**Phase 5 — Runtime metrics.** Thread-safe `Metrics` struct tracking STW pauses, heap peak, allocations, collections. Atomic counters for mutator-side stats, mutex for GC-side stats. Produces a formatted summary report.

**Phase 6 — Shared grey work queue.** Write barrier pushes greyed objects into `GreyQueue` via `GreyPusher` callback. GC drains the queue each iteration — O(k) per drain instead of O(n) heap scan. New objects born black during marking.

**Phase 7 — Concurrent sweep.** STW #2 shrinks to just disabling the barrier and building a sweep queue. Actual deletion happens concurrently in batches while mutators keep running.

**Phase 8 — Goroutine stack scanning.** Each mutator registers a `GoroutineStack` with local variable pointers. During STW #1, the GC scans all stacks to discover roots dynamically — no more hardcoded root IDs. Stack churn (dropping/picking up locals) means roots change between cycles.

**Phase 9 — GC assist.** Mutators that allocate during marking incur assist debt. Before the next allocation, they must mark grey objects to pay it off. Fast allocators are forced to help the GC keep up — backpressure that prevents the heap from running away.

**Phase 10 — Span memory model.** Objects live in fixed-size spans (8 slots each) with per-span mutexes. Sweep locks one span at a time — mutators allocate from other spans freely. Objects carry back-pointers to their span and slot index.

## GC Cycle Structure

The final collector runs four phases, matching real Go:

```
STW #1    Reset colors, enable write barrier, scan goroutine stacks, shade roots grey
  ↓
Mark      Concurrent — trace grey objects, drain barrier queue, mutators run freely
  ↓
STW #2    Disable barrier, reset assist debt, build span-based sweep queue
  ↓
Sweep     Concurrent — per-span locking, mutators interleave between spans
```

## What's Modeled vs Simplified

Semantically faithful to real Go: tri-color marking, hybrid write barrier, four-phase GC structure, GOGC pacing, stack-based root discovery, GC assist, span memory, born-black allocation.

Simplified compared to real Go: no size classes (all objects same size), no per-P mcache (we lock spans), no sweep-before-alloc (we sweep all dirty spans after marking), flat 1:1 assist ratio (real Go uses a dynamic ratio based on remaining work).

See [design_doc.md](design_doc.md) for the full phase-by-phase design with code snippets, diagrams, and comparisons to real Go internals.
