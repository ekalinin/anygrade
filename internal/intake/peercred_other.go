//go:build !linux && !darwin

package intake

import "net"

// peerUID has no portable implementation outside linux/darwin, so the socket
// mode is the only guard there; the caller degrades to accepting the peer.
func peerUID(*net.UnixConn) (uid int, supported bool, err error) {
	return 0, false, nil
}
