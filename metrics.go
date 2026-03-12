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
	gcCycles        int
	totalSTW1       time.Duration // cumulative STW mark-setup time
	totalSTW2       time.Duration // cumulative STW mark-termination time
	totalMarkTime   time.Duration // cumulative concurrent mark time
	totalSweepTime  time.Duration // cumulative concurrent sweep time
	totalGCTime     time.Duration // cumulative total GC time
	minSTW          time.Duration // shortest STW pause (either phase)
	maxSTW          time.Duration // longest STW pause (either phase)
	stwCount        int           // number of individual STW pauses (2 per cycle)

	// Stack scanning stats
	stacksScanned int // total goroutine stacks scanned across all cycles
	rootsFound    int // total root pointers found from stack scans

	// GC assist stats
	totalAssists    int64 // total number of assist events (mutator had to help)
	totalAssistWork int64 // total grey objects marked by mutators via assist

	// Span stats
	peakSpanCount int // max number of spans seen
	spansSwept    int // total spans swept across all cycles

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

// RecordSpanInfo records span statistics after a GC cycle.
func (m *Metrics) RecordSpanInfo(spanCount, spansSwept int) {
	m.mu.Lock()
	if spanCount > m.peakSpanCount {
		m.peakSpanCount = spanCount
	}
	m.spansSwept += spansSwept
	m.mu.Unlock()
}

// RecordAssist is called by a mutator when it was forced to do
// marking work before allocating (GC assist). objectsMarked is the
// number of grey objects the mutator marked black.
func (m *Metrics) RecordAssist(objectsMarked int) {
	m.mu.Lock()
	m.totalAssists++
	m.totalAssistWork += int64(objectsMarked)
	m.mu.Unlock()
}

// RecordStackScan records the result of scanning goroutine stacks
// during STW #1.
func (m *Metrics) RecordStackScan(numStacks, numRoots int) {
	m.mu.Lock()
	m.stacksScanned += numStacks
	m.rootsFound += numRoots
	m.mu.Unlock()
}

// RecordGCCycle is called after each completed GC cycle with the
// per-cycle stats and the post-sweep heap size.
func (m *Metrics) RecordGCCycle(stats GCStats) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.gcCycles++
	m.totalSTW1 += stats.STWMarkSetup
	m.totalSTW2 += stats.STWMarkTermination
	m.totalMarkTime += stats.ConcurrentMark
	m.totalSweepTime += stats.ConcurrentSweep
	m.totalGCTime += stats.TotalTime

	if stats.Collected > 0 {
		m.totalCollected.Add(int64(stats.Collected))
	}

	// Track min/max across both STW phases.
	for _, pause := range []time.Duration{stats.STWMarkSetup, stats.STWMarkTermination} {
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

	// Stack scanning
	if m.stacksScanned > 0 {
		b.WriteString("  Stack Scanning\n")
		b.WriteString(fmt.Sprintf("    Stacks scanned  : %d (across %d cycles)\n", m.stacksScanned, m.gcCycles))
		b.WriteString(fmt.Sprintf("    Roots found     : %d\n", m.rootsFound))
		if m.gcCycles > 0 {
			b.WriteString(fmt.Sprintf("    Avg roots/cycle : %d\n", m.rootsFound/m.gcCycles))
		}
		b.WriteString("\n")
	}

	// GC assist
	if m.totalAssists > 0 {
		b.WriteString("  GC Assist\n")
		b.WriteString(fmt.Sprintf("    Assist events   : %d\n", m.totalAssists))
		b.WriteString(fmt.Sprintf("    Objects marked  : %d (by mutators, not GC)\n", m.totalAssistWork))
		if m.totalAssists > 0 {
			b.WriteString(fmt.Sprintf("    Avg work/assist : %.1f objects\n",
				float64(m.totalAssistWork)/float64(m.totalAssists)))
		}
		b.WriteString("\n")
	}

	// STW pauses
	b.WriteString("  Stop-the-World Pauses\n")
	totalSTW := m.totalSTW1 + m.totalSTW2
	b.WriteString(fmt.Sprintf("    Total STW time  : %s\n", totalSTW))
	b.WriteString(fmt.Sprintf("      mark setup    : %s (across %d pauses)\n", m.totalSTW1, m.gcCycles))
	b.WriteString(fmt.Sprintf("      mark term     : %s (across %d pauses)\n", m.totalSTW2, m.gcCycles))
	if m.stwCount > 0 {
		avgSTW := totalSTW / time.Duration(m.stwCount)
		b.WriteString(fmt.Sprintf("    Avg STW pause   : %s\n", avgSTW))
		b.WriteString(fmt.Sprintf("    Min STW pause   : %s\n", m.minSTW))
		b.WriteString(fmt.Sprintf("    Max STW pause   : %s\n", m.maxSTW))
	}
	b.WriteString("\n")

	// Concurrent work
	b.WriteString("  Concurrent Work\n")
	b.WriteString(fmt.Sprintf("    Mark time       : %s\n", m.totalMarkTime))
	b.WriteString(fmt.Sprintf("    Sweep time      : %s\n", m.totalSweepTime))
	totalConc := m.totalMarkTime + m.totalSweepTime
	if m.totalGCTime > 0 {
		concPct := float64(totalConc) / float64(m.totalGCTime) * 100
		b.WriteString(fmt.Sprintf("    Concurrent %%    : %.1f%% of total GC time\n", concPct))
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

	// Spans
	if m.peakSpanCount > 0 {
		b.WriteString("  Spans\n")
		b.WriteString(fmt.Sprintf("    Peak span count : %d (×%d slots = %d capacity)\n",
			m.peakSpanCount, SpanSize, m.peakSpanCount*SpanSize))
		b.WriteString(fmt.Sprintf("    Spans swept     : %d (across %d cycles)\n", m.spansSwept, m.gcCycles))
		if m.gcCycles > 0 {
			b.WriteString(fmt.Sprintf("    Avg swept/cycle : %d spans\n", m.spansSwept/m.gcCycles))
		}
		b.WriteString("\n")
	}

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

