package main

import (
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// configureListener sets TCP_NOTSENT_LOWAT and TCP_USER_TIMEOUT on the
// listening socket before bind. Linux copies both onto accepted sockets.
func configureListener(lc *net.ListenConfig) {
	lc.Control = controlTCP
}

func controlTCP(_, _ string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, tcpNotSentLowat); e != nil {
			sockErr = e
			return
		}
		// Milliseconds. Same 30s as WriteTimeout: data that stays
		// unacknowledged, or buffered because the window is zero, aborts
		// the connection instead of holding it.
		ms := int(writeTimeout / time.Millisecond)
		if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, ms); e != nil {
			sockErr = e
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
