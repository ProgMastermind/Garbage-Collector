package gc

import (
	"testing"
	"time"
)

// TestPacerTriggersGC verifies that with GOGC=100, the pacer
// automatically triggers GC without any manual calls. Mutators
// allocate objects, the heap grows, and the pacer fires collection
// when heapSize >= liveAfterLastGC * 2.
func TestPacerTriggersGC(t *testing.T) {
	rootIDs := []int{0, 1, 2}
	stats := RunWithPacer(
		200,          // heapSize
		rootIDs,
		3,            // mutators
		100,          // GOGC
		500*time.Millisecond,
		false,
	)

	if len(stats) == 0 {
		t.Fatal("pacer never triggered GC — expected at least one cycle")
	}
	t.Logf("GOGC=100: %d GC cycles in 500ms", len(stats))
	for _, s := range stats {
		t.Logf("  %s", s)
	}
}

// TestGOGCAffectsFrequency verifies that a lower GOGC value causes
// the pacer to trigger collection sooner. We simulate a steady
// allocation stream and count how many times ShouldCollect fires
// for different GOGC values — no goroutines, no timing variance.
func TestGOGCAffectsFrequency(t *testing.T) {
	// Simulate: start with 10 live objects after GC, then allocate
	// one at a time. Count how many allocations until the pacer fires.
	triggerPoint := func(gogc int) int {
		p := NewPacer(gogc)
		p.RecordCycle(10) // 10 objects survived last GC
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
	t.Logf("GOGC=100: triggers at heap size %d (goal: 10 * 2.0  = 20)",   trigger100)
	t.Logf("GOGC=200: triggers at heap size %d (goal: 10 * 3.0  = 30)",   trigger200)

	// GOGC=25 should trigger much sooner than GOGC=100.
	if trigger25 >= trigger100 {
		t.Errorf("GOGC=25 should trigger before GOGC=100: %d vs %d", trigger25, trigger100)
	}
	// GOGC=100 should trigger much sooner than GOGC=200.
	if trigger100 >= trigger200 {
		t.Errorf("GOGC=100 should trigger before GOGC=200: %d vs %d", trigger100, trigger200)
	}
	// The ratio should roughly match the GOGC ratio.
	// GOGC=200 allows 3x growth, GOGC=25 allows 1.25x → ~2.4x difference.
	if trigger200 < 2*trigger25 {
		t.Errorf("expected GOGC=200 trigger point to be at least 2x GOGC=25: %d vs %d",
			trigger200, trigger25)
	}
}

// TestGOGCZero verifies that GOGC=0 means "collect constantly."
// The goal becomes liveAfterLastGC * 1.0, so any growth at all
// triggers collection.
func TestGOGCZero(t *testing.T) {
	// Deterministic check: with 10 live objects after GC, GOGC=0
	// triggers the moment heap reaches 10 (i.e., any allocation
	// above the live set).
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

	// Integration check: GOGC=0 fires at least once with real mutators.
	rootIDs := []int{0, 1, 2}
	stats := RunWithPacer(200, rootIDs, 3, 0, 300*time.Millisecond, false)
	if len(stats) == 0 {
		t.Fatal("GOGC=0 triggered zero cycles with real mutators — expected many")
	}
	t.Logf("GOGC=0: %d cycles in 300ms", len(stats))
}
