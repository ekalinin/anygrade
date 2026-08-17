//go:build darwin

package intake

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the uid of the process on the other end of conn via
// LOCAL_PEERCRED, darwin's counterpart of SO_PEERCRED.
func peerUID(conn *net.UnixConn) (uid int, supported bool, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, true, err
	}
	var cred *unix.Xucred
	var soErr error
	if err := raw.Control(func(fd uintptr) {
		cred, soErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, true, err
	}
	if soErr != nil {
		return 0, true, soErr
	}
	return int(cred.Uid), true, nil
}
