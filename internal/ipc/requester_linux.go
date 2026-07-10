//go:build linux

package ipc

import (
	"net"
	"syscall"
)

func peerPIDForConn(conn net.Conn) int {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int
	if err := raw.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err == nil {
			pid = int(cred.Pid)
		}
	}); err != nil {
		return 0
	}
	return pid
}
