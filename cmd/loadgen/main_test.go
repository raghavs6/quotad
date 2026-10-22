package main

import (
	"testing"
	"time"
)

func TestUtilizationSkipsWindowCutOffByRunEnd(t *testing.T) {
	end := time.Unix(1180, 0)
	resets := []int64{1060, 1120, 1180, 1200} // the last one resets after the run ends
	oks := []int{100, 90, 80, 7}
	if got := utilization(resets, oks, 100, end); got != 0.9 {
		t.Fatalf("utilization = %v, want 0.9 (270 ok over 3 full windows of 100)", got)
	}
}
