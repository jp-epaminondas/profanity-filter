//go:build windows

package clock

import (
	"syscall"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	procQPC  = kernel32.NewProc("QueryPerformanceCounter")
	procQPF  = kernel32.NewProc("QueryPerformanceFrequency")

	// frequency is the number of counter ticks per second.
	frequency = queryFrequency()
)

func queryFrequency() int64 {
	var f int64
	procQPF.Call(uintptr(unsafe.Pointer(&f)))
	if f <= 0 {
		panic("clock: QueryPerformanceFrequency unavailable")
	}
	return f
}

func ticks() int64 {
	var c int64
	procQPC.Call(uintptr(unsafe.Pointer(&c)))
	return c
}
