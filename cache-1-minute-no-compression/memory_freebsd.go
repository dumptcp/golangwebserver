package main

import "golang.org/x/sys/unix"

// physicalMemory is the machine's RAM in bytes (hw.physmem).
func physicalMemory() uint64 {
	n, err := unix.SysctlUint64("hw.physmem")
	if err != nil {
		return 0
	}
	return n
}

func cgroupMemoryLimit() uint64 { return 0 }
