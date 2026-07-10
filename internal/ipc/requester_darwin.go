//go:build darwin

package ipc

import (
	"net"
	"syscall"
	"unsafe"
)

const localPeerPID = 2

func peerPIDForConn(conn net.Conn) int {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int32
	size := uint32(unsafe.Sizeof(pid))
	if err := raw.Control(func(fd uintptr) {
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			0,
			localPeerPID,
			uintptr(unsafe.Pointer(&pid)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			pid = 0
		}
	}); err != nil {
		return 0
	}
	return int(pid)
}
