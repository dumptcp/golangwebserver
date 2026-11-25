//go:build !linux

package main

import "net"

// configureListener is a no-op where TCP_NOTSENT_LOWAT and TCP_USER_TIMEOUT
// are not available. The write timeout still closes a stalled response.
func configureListener(*net.ListenConfig) {}
