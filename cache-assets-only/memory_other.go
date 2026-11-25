//go:build !linux && !darwin && !freebsd && !windows

package main

func physicalMemory() uint64    { return 0 }
func cgroupMemoryLimit() uint64 { return 0 }
