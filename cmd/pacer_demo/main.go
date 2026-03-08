package main

import (
	"fmt"
	"gc-demo"
	"time"
)

func main() {
	rootIDs := []int{0, 1, 2}
	dur := 3 * time.Second

	fmt.Println("========== GOGC=25 (aggressive — collect early) ==========")
	stats25 := gc.RunWithPacer(500, rootIDs, 2, 25, dur, true)
	fmt.Printf("Total cycles: %d\n\n", len(stats25))

	fmt.Println("========== GOGC=200 (relaxed — let heap grow) ==========")
	stats200 := gc.RunWithPacer(500, rootIDs, 2, 200, dur, true)
	fmt.Printf("Total cycles: %d\n", len(stats200))
}
