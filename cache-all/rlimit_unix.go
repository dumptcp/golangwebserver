//go:build unix

package main

import (
	"log"
	"os/signal"
	"syscall"
)

func ignoreSIGPIPE() { signal.Ignore(syscall.SIGPIPE) }

// raiseOpenFileLimit raises the soft RLIMIT_NOFILE to the hard limit when
// the soft limit is lower. It returns the soft limit in effect, or 0 when
// the limit cannot be read.
func raiseOpenFileLimit() uint64 {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		log.Printf("could not read open file limit: %v", err)
		return 0
	}
	if lim.Cur < lim.Max {
		want := lim
		want.Cur = want.Max
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &want); err != nil {
			log.Printf("could not raise open file limit: %v", err)
		} else if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			log.Printf("could not read open file limit: %v", err)
			return rlimCur(want.Cur)
		}
	}
	log.Printf("open file limit: %d", rlimCur(lim.Cur))
	return rlimCur(lim.Cur)
}

// rlimCur converts the platform rlimit field (uint64 on Linux and macOS,
// int64 on FreeBSD) to a byte count. A negative unlimited sentinel becomes
// the maximum uint64.
func rlimCur[T ~int64 | ~uint64](v T) uint64 {
	if v < 0 {
		return ^uint64(0)
	}
	return uint64(v)
}
