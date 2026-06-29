//go:build linux

package ipc

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

func requesterForConn(conn net.Conn) requester {
	fallback := requester{Role: "manager"}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fallback
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return fallback
	}
	var pid int
	if err := raw.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err == nil {
			pid = int(cred.Pid)
		}
	}); err != nil || pid == 0 {
		return fallback
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return fallback
	}
	return requesterFromEnv(environ)
}
