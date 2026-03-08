package gc

import (
	"sync"
	"testing"
	"time"
)

// runWithPacer wraps RunWithPacer, discarding metrics for Phase 4 tests.
func runWithPacer(heapSize int, rootIDs []int, numMutators, gogc int, duration time.Duration, printStats bool) []GCStats {
	stats, _ := RunWithPacer(heapSize, rootIDs, numMutators, gogc, duration, printStats)
	return stats
}

// --- Phase 4 tests ---

func TestPacerTriggersGC(t *testing.T) {
	rootIDs := []int{0, 1, 2}
	stats := runWithPacer(200, rootIDs, 3, 100, 500*time.Millisecond, false)
	if len(stats) == 0 {
		t.Fatal("pacer never triggered GC — expected at least one cycle")
	}
	t.Logf("GOGC=100: %d GC cycles in 500ms", len(stats))
	for _, s := range stats {
		t.Logf("  %s", s)
	}
}

func TestGOGCAffectsFrequency(t *testing.T) {
	triggerPoint := func(gogc int) int {
		p := NewPacer(gogc)
		p.RecordCycle(10)
		for allocs := 11; allocs < 1000; allocs++ {
			if p.ShouldCollect(allocs) {
				return allocs
			}
		}
		return -1
	}

	trigger25 := triggerPoint(25)
	trigger100 := triggerPoint(100)
	trigger200 := triggerPoint(200)

	t.Logf("GOGC=25:  triggers at heap size %d (goal: 10 * 1.25 = 12.5)", trigger25)
	t.Logf("GOGC=100: triggers at heap size %d (goal: 10 * 2.0  = 20)", trigger100)
	t.Logf("GOGC=200: triggers at heap size %d (goal: 10 * 3.0  = 30)", trigger200)

	if trigger25 >= trigger100 {
		t.Errorf("GOGC=25 should trigger before GOGC=100: %d vs %d", trigger25, trigger100)
	}
	if trigger100 >= trigger200 {
		t.Errorf("GOGC=100 should trigger before GOGC=200: %d vs %d", trigger100, trigger200)
	}
	if trigger200 < 2*trigger25 {
		t.Errorf("expected GOGC=200 trigger point to be at least 2x GOGC=25: %d vs %d",
			trigger200, trigger25)
	}
}

func TestGOGCZero(t *testing.T) {
	p := NewPacer(0)
	p.RecordCycle(10)

	if p.ShouldCollect(9) {
		t.Error("GOGC=0: should not trigger below live set")
	}
	if !p.ShouldCollect(10) {
		t.Error("GOGC=0: should trigger at exactly the live set size")
	}
	if !p.ShouldCollect(11) {
		t.Error("GOGC=0: should trigger above the live set size")
	}

	rootIDs := []int{0, 1, 2}
	stats := runWithPacer(200, rootIDs, 3, 0, 300*time.Millisecond, false)
	if len(stats) == 0 {
		t.Fatal("GOGC=0 triggered zero cycles with real mutators — expected many")
	}
	t.Logf("GOGC=0: %d cycles in 300ms", len(stats))
}

// --- Phase 5 tests ---

// TestMetricsAccuracy runs a known workload and verifies that metrics
// are internally consistent.
func TestMetricsAccuracy(t *testing.T) {
	rootIDs := []int{0, 1, 2}
	_, metrics := RunWithPacer(
		200, rootIDs, 3, 100,
		1*time.Second, false,
	)

	// Must have run at least one GC cycle.
	if metrics.GCCycles() == 0 {
		t.Fatal("expected at least one GC cycle")
	}

	// Total collected must not exceed total allocated.
	alloc := metrics.TotalAllocated()
	collected := metrics.TotalCollected()
	t.Logf("allocated=%d collected=%d cycles=%d peak=%d",
		alloc, collected, metrics.GCCycles(), metrics.PeakHeapSize())

	if collected > alloc {
		t.Errorf("collected (%d) exceeds allocated (%d)", collected, alloc)
	}

	// Peak heap must not exceed capacity.
	if metrics.PeakHeapSize() > 200 {
		t.Errorf("peak heap (%d) exceeds capacity (200)", metrics.PeakHeapSize())
	}

	// Min STW must be <= Max STW.
	if metrics.MinSTW() > metrics.MaxSTW() {
		t.Errorf("min STW (%s) > max STW (%s)", metrics.MinSTW(), metrics.MaxSTW())
	}

	// Total allocated must be positive (mutators were running).
	if alloc == 0 {
		t.Error("expected allocations from mutators, got 0")
	}
}

// TestMetricsConcurrentSafety hammers Metrics from 8 goroutines
// simultaneously. Run with -race to verify no data races.
func TestMetricsConcurrentSafety(t *testing.T) {
	m := NewMetrics(1000)
	var wg sync.WaitGroup
	const goroutines = 8
	const iterations = 1000

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				m.RecordAllocation()
				m.ObserveHeapSize(500)
				m.RecordGCCycle(GCStats{
					Cycle:          1,
					HeapBefore:     400,
					HeapAfter:      300,
					Collected:      100,
					TotalTime:      10 * time.Millisecond,
					STWMarkSetup:   5 * time.Microsecond,
					ConcurrentMark: 9 * time.Millisecond,
					STWSweep:       3 * time.Microsecond,
				})
			}
		}()
	}
	wg.Wait()

	// Verify counts are consistent.
	expectedAllocs := int64(goroutines * iterations)
	if m.TotalAllocated() != expectedAllocs {
		t.Errorf("expected %d allocations, got %d", expectedAllocs, m.TotalAllocated())
	}

	expectedCycles := goroutines * iterations
	if m.GCCycles() != expectedCycles {
		t.Errorf("expected %d GC cycles, got %d", expectedCycles, m.GCCycles())
	}

	expectedCollected := int64(goroutines * iterations * 100)
	if m.TotalCollected() != expectedCollected {
		t.Errorf("expected %d collected, got %d", expectedCollected, m.TotalCollected())
	}
}
