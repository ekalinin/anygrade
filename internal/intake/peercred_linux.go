//go:build linux

package intake

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the uid of the process on the other end of conn via
// SO_PEERCRED.
func peerUID(conn *net.UnixConn) (uid int, supported bool, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, true, err
	}
	var cred *unix.Ucred
	var soErr error
	if err := raw.Control(func(fd uintptr) {
		cred, soErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, true, err
	}
	if soErr != nil {
		return 0, true, soErr
	}
	return int(cred.Uid), true, nil
}
