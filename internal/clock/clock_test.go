package clock

import (
	"testing"
	"time"
)

func TestStopwatchResolution(t *testing.T) {
	// Busy-wait a few microseconds: a coarse clock would report 0 here.
	sw := Start()
	deadline := time.Now().Add(50 * time.Millisecond)
	for sw.Elapsed() < 5*time.Microsecond {
		if time.Now().After(deadline) {
			t.Fatal("stopwatch did not advance within 50ms")
		}
	}
}

func TestStopwatchMatchesWallClock(t *testing.T) {
	sw := Start()
	time.Sleep(20 * time.Millisecond)
	got := sw.Elapsed()
	if got < 15*time.Millisecond || got > 500*time.Millisecond {
		t.Errorf("Elapsed() = %v after sleeping 20ms", got)
	}
}
