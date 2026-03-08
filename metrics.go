package gc

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics tracks runtime statistics across an entire GC-enabled run.
// All methods are safe for concurrent use from mutator and GC goroutines.
type Metrics struct {
	mu sync.Mutex

	// GC cycle stats
	gcCycles       int
	totalSTW1      time.Duration // cumulative STW mark-setup time
	totalSTW2      time.Duration // cumulative STW sweep time
	totalMarkTime  time.Duration // cumulative concurrent mark time
	totalGCTime    time.Duration // cumulative total GC time
	minSTW         time.Duration // shortest STW pause (either phase)
	maxSTW         time.Duration // longest STW pause (either phase)
	stwCount       int           // number of individual STW pauses (2 per cycle)

	// Heap stats
	peakHeapSize int // max heap occupancy seen
	heapCapacity int // max allowed objects

	// Object stats — atomics so mutators can bump without locking
	totalAllocated atomic.Int64
	totalCollected atomic.Int64

	// Snapshot of last known heap size (updated by GC and pacer)
	lastHeapSize int
}

func NewMetrics(heapCapacity int) *Metrics {
	return &Metrics{
		heapCapacity: heapCapacity,
		minSTW:       time.Duration(1<<63 - 1), // max duration, so first real value wins
	}
}

// RecordAllocation is called by mutators after each successful Alloc.
func (m *Metrics) RecordAllocation() {
	m.totalAllocated.Add(1)
}

// RecordGCCycle is called after each completed GC cycle with the
// per-cycle stats and the post-sweep heap size.
func (m *Metrics) RecordGCCycle(stats GCStats) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.gcCycles++
	m.totalSTW1 += stats.STWMarkSetup
	m.totalSTW2 += stats.STWSweep
	m.totalMarkTime += stats.ConcurrentMark
	m.totalGCTime += stats.TotalTime

	if stats.Collected > 0 {
		m.totalCollected.Add(int64(stats.Collected))
	}

	// Track min/max across both STW phases.
	for _, pause := range []time.Duration{stats.STWMarkSetup, stats.STWSweep} {
		m.stwCount++
		if pause < m.minSTW {
			m.minSTW = pause
		}
		if pause > m.maxSTW {
			m.maxSTW = pause
		}
	}

	// Update peak heap from before-GC snapshot (the high-water mark).
	if stats.HeapBefore > m.peakHeapSize {
		m.peakHeapSize = stats.HeapBefore
	}
	// Also check after — in case mutators pushed it higher during mark.
	if stats.HeapAfter > m.peakHeapSize {
		m.peakHeapSize = stats.HeapAfter
	}

	m.lastHeapSize = stats.HeapAfter
}

// ObserveHeapSize is called by the pacer loop to track peak heap
// between GC cycles.
func (m *Metrics) ObserveHeapSize(size int) {
	m.mu.Lock()
	if size > m.peakHeapSize {
		m.peakHeapSize = size
	}
	m.lastHeapSize = size
	m.mu.Unlock()
}

// Summary returns a formatted multi-line report of all metrics.
func (m *Metrics) Summary(gogc int, duration time.Duration, numMutators int) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder

	line := strings.Repeat("─", 60)
	b.WriteString("\n" + line + "\n")
	b.WriteString("  GC Simulation Summary\n")
	b.WriteString(line + "\n\n")

	// Config
	b.WriteString("  Configuration\n")
	b.WriteString(fmt.Sprintf("    GOGC            : %d\n", gogc))
	b.WriteString(fmt.Sprintf("    Heap capacity   : %d objects\n", m.heapCapacity))
	b.WriteString(fmt.Sprintf("    Mutators        : %d\n", numMutators))
	b.WriteString(fmt.Sprintf("    Duration        : %s\n", duration))
	b.WriteString("\n")

	// GC cycles
	b.WriteString("  Collection\n")
	b.WriteString(fmt.Sprintf("    GC cycles       : %d\n", m.gcCycles))
	b.WriteString(fmt.Sprintf("    Total GC time   : %s\n", m.totalGCTime))
	if m.gcCycles > 0 {
		avgTotal := m.totalGCTime / time.Duration(m.gcCycles)
		b.WriteString(fmt.Sprintf("    Avg cycle time  : %s\n", avgTotal))
	}
	b.WriteString("\n")

	// STW pauses
	b.WriteString("  Stop-the-World Pauses\n")
	b.WriteString(fmt.Sprintf("    Total STW time  : %s\n", m.totalSTW1+m.totalSTW2))
	b.WriteString(fmt.Sprintf("      mark setup    : %s (across %d pauses)\n", m.totalSTW1, m.gcCycles))
	b.WriteString(fmt.Sprintf("      sweep         : %s (across %d pauses)\n", m.totalSTW2, m.gcCycles))
	if m.stwCount > 0 {
		avgSTW := (m.totalSTW1 + m.totalSTW2) / time.Duration(m.stwCount)
		b.WriteString(fmt.Sprintf("    Avg STW pause   : %s\n", avgSTW))
		b.WriteString(fmt.Sprintf("    Min STW pause   : %s\n", m.minSTW))
		b.WriteString(fmt.Sprintf("    Max STW pause   : %s\n", m.maxSTW))
	}
	b.WriteString("\n")

	// Concurrent mark
	b.WriteString("  Concurrent Mark\n")
	b.WriteString(fmt.Sprintf("    Total mark time : %s\n", m.totalMarkTime))
	if m.gcCycles > 0 {
		avgMark := m.totalMarkTime / time.Duration(m.gcCycles)
		b.WriteString(fmt.Sprintf("    Avg mark time   : %s\n", avgMark))
	}
	if m.totalGCTime > 0 {
		concPct := float64(m.totalMarkTime) / float64(m.totalGCTime) * 100
		b.WriteString(fmt.Sprintf("    Mark %% of GC    : %.1f%%\n", concPct))
	}
	b.WriteString("\n")

	// Heap
	b.WriteString("  Heap\n")
	b.WriteString(fmt.Sprintf("    Peak heap size  : %d objects\n", m.peakHeapSize))
	b.WriteString(fmt.Sprintf("    Final heap size : %d objects\n", m.lastHeapSize))
	if m.heapCapacity > 0 {
		utilPct := float64(m.lastHeapSize) / float64(m.heapCapacity) * 100
		b.WriteString(fmt.Sprintf("    Utilization     : %.1f%% (%d / %d)\n",
			utilPct, m.lastHeapSize, m.heapCapacity))
	}
	b.WriteString("\n")

	// Objects
	alloc := m.totalAllocated.Load()
	collected := m.totalCollected.Load()
	b.WriteString("  Objects\n")
	b.WriteString(fmt.Sprintf("    Total allocated : %d\n", alloc))
	b.WriteString(fmt.Sprintf("    Total collected : %d\n", collected))
	if alloc > 0 {
		collectPct := float64(collected) / float64(alloc) * 100
		b.WriteString(fmt.Sprintf("    Collection rate : %.1f%%\n", collectPct))
	}

	b.WriteString("\n" + line + "\n")
	return b.String()
}

// --- Accessors for testing ---

func (m *Metrics) GCCycles() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gcCycles
}

func (m *Metrics) PeakHeapSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peakHeapSize
}

func (m *Metrics) TotalAllocated() int64 {
	return m.totalAllocated.Load()
}

func (m *Metrics) TotalCollected() int64 {
	return m.totalCollected.Load()
}

func (m *Metrics) MinSTW() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.minSTW
}

func (m *Metrics) MaxSTW() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxSTW
}
