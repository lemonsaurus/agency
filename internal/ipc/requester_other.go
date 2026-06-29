//go:build !linux

package ipc

import "net"

func requesterForConn(_ net.Conn) requester {
	return requester{Role: "manager"}
}
