package gc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// GCStats records what happened during one collection cycle.
type GCStats struct {
	Cycle              int
	HeapBefore         int
	HeapAfter          int
	Collected          int
	TotalTime          time.Duration // wall-clock time for entire GC cycle
	STWMarkSetup       time.Duration // STW pause #1: reset colors, enable barrier, shade roots
	ConcurrentMark     time.Duration // concurrent: trace the object graph (mutators run freely)
	STWMarkTermination time.Duration // STW pause #2: disable barrier, build sweep queue
	ConcurrentSweep    time.Duration // concurrent: free white objects (mutators run freely)
}

func (s GCStats) String() string {
	return fmt.Sprintf(
		"[cycle %d] before=%d after=%d collected=%d | total=%s stw1=%s mark=%s stw2=%s sweep=%s",
		s.Cycle, s.HeapBefore, s.HeapAfter, s.Collected,
		s.TotalTime, s.STWMarkSetup, s.ConcurrentMark,
		s.STWMarkTermination, s.ConcurrentSweep,
	)
}

// Pacer decides when to trigger GC based on heap growth relative to
// the last post-GC live set, controlled by GOGC.
//
//	trigger when: heapSize >= liveAfterLastGC * (1 + GOGC/100)
//
// GOGC=100 (default): collect when heap doubles since last GC.
// GOGC=0: collect constantly (after every allocation batch).
type Pacer struct {
	GOGC            int // percentage growth allowed before next GC
	liveAfterLastGC int // objects surviving the most recent collection
	gcCycles        atomic.Int64
}

func NewPacer(gogc int) *Pacer {
	return &Pacer{
		GOGC:            gogc,
		liveAfterLastGC: 1, // bootstrap: treat initial live set as 1
	}
}

// ShouldCollect returns true when the heap has grown enough to warrant
// a new GC cycle.
func (p *Pacer) ShouldCollect(heapSize int) bool {
	goal := float64(p.liveAfterLastGC) * (1.0 + float64(p.GOGC)/100.0)
	return float64(heapSize) >= goal
}

// RecordCycle updates the pacer after a collection finishes.
func (p *Pacer) RecordCycle(liveAfter int) {
	p.liveAfterLastGC = liveAfter
	if p.liveAfterLastGC < 1 {
		p.liveAfterLastGC = 1 // avoid triggering on empty heap forever
	}
	p.gcCycles.Add(1)
}

// Cycles returns the number of completed GC cycles.
func (p *Pacer) Cycles() int {
	return int(p.gcCycles.Load())
}

// RunWithPacer starts mutators and a pacer loop that triggers GC based
// on heap occupancy. It runs for the given duration, then stops
// everything and returns the collected stats and metrics.
func RunWithPacer(
	heapSize int,
	rootIDs []int,
	numMutators int,
	gogc int,
	duration time.Duration,
	printStats bool,
) ([]GCStats, *Metrics) {
	ch := NewConcurrentHeap(heapSize)
	pacer := NewPacer(gogc)
	metrics := NewMetrics(heapSize)

	// Pre-create root objects (placed into spans via Insert).
	ch.mu.Lock()
	for _, id := range rootIDs {
		obj := &Object{ID: id, Color: White}
		ch.Heap.Insert(obj)
	}
	ch.mu.Unlock()

	// Start mutators — pass metrics so they can report allocations.
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range numMutators {
		wg.Add(1)
		go MutatorWithMetrics(ch, rootIDs, done, &wg, metrics)
	}

	var allStats []GCStats
	cycle := 0
	deadline := time.After(duration)

	// Pacer loop: check heap size, trigger GC when the pacing
	// condition is met.
	for {
		select {
		case <-deadline:
			close(done)
			wg.Wait()
			return allStats, metrics
		default:
		}

		ch.mu.Lock()
		heapNow := len(ch.Heap.Objects)
		shouldRun := pacer.ShouldCollect(heapNow)
		before := heapNow
		ch.mu.Unlock()

		// Let metrics track peak heap between GC cycles.
		metrics.ObserveHeapSize(before)

		if shouldRun {
			cycleStart := time.Now()

			timings := ConcurrentMarkSweep(ch, rootIDs)

			ch.mu.Lock()
			after := len(ch.Heap.Objects)
			spanCount := len(ch.Heap.Spans)
			ch.mu.Unlock()

			collected := before - after
			if collected < 0 {
				collected = 0
			}

			stats := GCStats{
				Cycle:              cycle,
				HeapBefore:         before,
				HeapAfter:          after,
				Collected:          collected,
				TotalTime:          time.Since(cycleStart),
				STWMarkSetup:       timings.STWMarkSetup,
				ConcurrentMark:     timings.ConcurrentMark,
				STWMarkTermination: timings.STWMarkTermination,
				ConcurrentSweep:    timings.ConcurrentSweep,
			}
			allStats = append(allStats, stats)
			metrics.RecordStackScan(timings.StacksScanned, timings.RootsFromStacks)
			metrics.RecordSpanInfo(spanCount, timings.SpansSwept)
			metrics.RecordGCCycle(stats)

			if printStats {
				fmt.Println(stats)
			}

			pacer.RecordCycle(after)
			cycle++
		}

		time.Sleep(50 * time.Microsecond)
	}
}

