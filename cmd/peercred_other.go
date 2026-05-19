//go:build !linux

package cmd

import (
	"errors"
	"net"
)

func readUnixPeerCred(_ *net.UnixConn) (peerCred, error) {
	return peerCred{}, errors.New("peer credentials unsupported on this platform")
}
