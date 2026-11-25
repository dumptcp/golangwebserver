package main

import (
	"syscall"
	"unsafe"
)

var procGlobalMemoryStatusEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// physicalMemory is the machine's RAM in bytes.
func physicalMemory() uint64 {
	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return 0
	}
	return st.TotalPhys
}

func cgroupMemoryLimit() uint64 { return 0 }
