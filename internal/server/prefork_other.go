//go:build !unix

package server

import (
	"errors"
	"net"
)

// reusePortListen is unavailable off unix; fasthttp fell back to SO_REUSEADDR on
// Windows, which does not give the same accept semantics, so prefork is refused.
func reusePortListen(_, _ string) (net.Listener, error) {
	return nil, errors.New("SO_REUSEPORT is not available on this platform")
}
