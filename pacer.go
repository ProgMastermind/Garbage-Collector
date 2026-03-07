package gc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// GCStats records what happened during one collection cycle.
type GCStats struct {
	Cycle          int
	HeapBefore     int
	HeapAfter      int
	Collected      int
	STWPause       time.Duration
}

func (s GCStats) String() string {
	return fmt.Sprintf(
		"[cycle %d] before=%d after=%d collected=%d pause=%s",
		s.Cycle, s.HeapBefore, s.HeapAfter, s.Collected, s.STWPause,
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
	GOGC           int // percentage growth allowed before next GC
	liveAfterLastGC int // objects surviving the most recent collection
	gcCycles       atomic.Int64
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
// everything and returns the collected stats.
func RunWithPacer(
	heapSize int,
	rootIDs []int,
	numMutators int,
	gogc int,
	duration time.Duration,
	printStats bool,
) []GCStats {
	ch := NewConcurrentHeap(heapSize)
	pacer := NewPacer(gogc)

	// Pre-create root objects.
	ch.mu.Lock()
	for _, id := range rootIDs {
		obj := &Object{ID: id, Color: White}
		ch.Heap.Objects[id] = obj
		if id >= ch.Heap.nextID {
			ch.Heap.nextID = id + 1
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
			return allStats
		default:
		}

		ch.mu.Lock()
		heapNow := len(ch.Heap.Objects)
		shouldRun := pacer.ShouldCollect(heapNow)
		before := heapNow
		ch.mu.Unlock()

		if shouldRun {
			stwStart := time.Now()

			ConcurrentMarkSweep(ch, rootIDs)

			// Snapshot heap size immediately after sweep while still
			// under lock, to avoid counting mutator allocations as
			// "surviving" objects.
			ch.mu.Lock()
			after := len(ch.Heap.Objects)
			ch.mu.Unlock()

			stwDur := time.Since(stwStart)

			collected := before - after
			if collected < 0 {
				collected = 0 // mutators allocated more than GC freed
			}

			stats := GCStats{
				Cycle:      cycle,
				HeapBefore: before,
				HeapAfter:  after,
				Collected:  collected,
				STWPause:   stwDur,
			}
			allStats = append(allStats, stats)
			if printStats {
				fmt.Println(stats)
			}

			pacer.RecordCycle(after)
			cycle++
		}

		time.Sleep(50 * time.Microsecond)
	}
}
