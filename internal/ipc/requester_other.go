//go:build !linux && !darwin

package ipc

import "net"

func peerPIDForConn(_ net.Conn) int {
	return 0
}
