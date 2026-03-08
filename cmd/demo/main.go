package main

import (
	"fmt"
	"gc-demo"
	"time"
)

func main() {
	fmt.Println("Go GC Simulation — Tri-Color Concurrent Mark & Sweep")
	fmt.Println()

	rootIDs := []int{0, 1, 2}
	gogc := 100
	heapCapacity := 500
	numMutators := 2
	dur := 5 * time.Second

	fmt.Printf("Running %d mutators for %s with GOGC=%d, heap capacity=%d ...\n\n",
		numMutators, dur, gogc, heapCapacity)

	stats, metrics := gc.RunWithPacer(
		heapCapacity,
		rootIDs,
		numMutators,
		gogc,
		dur,
		true, // print per-cycle stats as they happen
	)

	_ = stats
	fmt.Print(metrics.Summary(gogc, dur, numMutators))
}
