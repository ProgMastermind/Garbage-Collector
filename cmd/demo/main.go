package main

import (
	"fmt"
	"gc-demo"
	"time"
)

func run(gogc int) {
	rootIDs := []int{0, 1, 2}
	heapCapacity := 500
	numMutators := 2
	dur := 5 * time.Second

	fmt.Printf("Running %d mutators for %s with GOGC=%d, heap capacity=%d ...\n\n",
		numMutators, dur, gogc, heapCapacity)

	_, metrics := gc.RunWithPacer(
		heapCapacity,
		rootIDs,
		numMutators,
		gogc,
		dur,
		false,
	)

	fmt.Print(metrics.Summary(gogc, dur, numMutators))
}

func main() {
	fmt.Println("Go GC Simulation — Tri-Color Concurrent Mark & Sweep")
	fmt.Println()

	fmt.Println("==================== GOGC=10 ====================")
	run(10)

	fmt.Println("\n==================== GOGC=1000 ====================")
	run(1000)
}
