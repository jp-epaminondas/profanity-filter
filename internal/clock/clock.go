// Package clock provides a high-resolution stopwatch for timing operations
// that take only a few microseconds.
//
// On Linux and macOS, time.Now already reads a nanosecond-resolution
// monotonic clock. On Windows, however, Go's monotonic clock advances only
// once per system timer tick (0.5 ms to 15.6 ms), so timing a 3 µs operation
// with time.Since usually yields "0s". There, this package reads
// QueryPerformanceCounter instead, which typically ticks every 100 ns.
package clock

import "time"

// Stopwatch measures elapsed time since it was started.
type Stopwatch struct {
	start int64 // in ticks of the platform counter
}

// Start returns a running Stopwatch.
func Start() Stopwatch { return Stopwatch{start: ticks()} }

// Elapsed returns the time since Start.
func (s Stopwatch) Elapsed() time.Duration {
	d := ticks() - s.start
	// Split the conversion to avoid overflowing int64 on long intervals.
	return time.Duration(d/frequency)*time.Second +
		time.Duration(d%frequency)*time.Second/time.Duration(frequency)
}
