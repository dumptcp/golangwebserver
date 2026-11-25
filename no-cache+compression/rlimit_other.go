//go:build !unix

package main

func ignoreSIGPIPE() {}

// raiseOpenFileLimit is a no-op where RLIMIT_NOFILE does not exist.
func raiseOpenFileLimit() uint64 { return 0 }
