//go:build !windows

package clock

import "time"

// frequency is the number of ticks per second: here, nanoseconds.
const frequency = int64(time.Second)

// epoch anchors the monotonic clock; time.Since reads it with ns resolution.
var epoch = time.Now()

func ticks() int64 { return int64(time.Since(epoch)) }
