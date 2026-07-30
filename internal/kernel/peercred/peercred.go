// Package peercred authenticates local Unix-socket peers using credentials
// attached by the host kernel to the connected file descriptor.
package peercred

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// UID returns the effective UID authenticated by the kernel for connection's
// remote peer. It fails closed for transports without a Unix descriptor.
func UID(connection net.Conn) (uint32, error) {
	syscallConnection, ok := connection.(syscall.Conn)
	if !ok {
		return 0, errors.New(
			"connection does not expose a Unix descriptor",
		)
	}
	rawConnection, err := syscallConnection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		uid           uint32
		credentialErr error
	)
	if err := rawConnection.Control(func(descriptor uintptr) {
		uid, credentialErr = uidFromDescriptor(int(descriptor))
	}); err != nil {
		return 0, err
	}
	if credentialErr != nil {
		return 0, credentialErr
	}
	return uid, nil
}

// FileOwnerUID returns the owner UID from Unix filesystem metadata.
func FileOwnerUID(info os.FileInfo) (uint32, error) {
	if info == nil {
		return 0, errors.New("Unix file metadata is unavailable")
	}
	uid, err := fileOwnerUID(info)
	if err != nil {
		return 0, fmt.Errorf("read Unix owner UID: %w", err)
	}
	return uid, nil
}
