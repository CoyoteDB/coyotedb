//go:build windows

package mono

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// QueryInterruptTime exists on Windows 10 and later (the oldest Windows
// Go supports).
var procQueryInterruptTime = windows.NewLazySystemDLL("kernelbase.dll").NewProc("QueryInterruptTime")

func now() time.Duration {
	var t uint64 // 100 ns units since boot, including sleep and hibernation
	procQueryInterruptTime.Call(uintptr(unsafe.Pointer(&t)))
	return time.Duration(t)*100 + 1
}
