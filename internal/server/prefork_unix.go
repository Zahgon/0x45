//go:build unix

package server

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePortListen binds addr with SO_REUSEPORT so every prefork child can share it.
func reusePortListen(network, addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var opErr error
			if err := c.Control(func(fd uintptr) {
				opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return opErr
		},
	}
	return lc.Listen(context.Background(), network, addr)
}
