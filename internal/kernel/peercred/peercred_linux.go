//go:build linux

package peercred

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func uidFromDescriptor(descriptor int) (uint32, error) {
	credential, err := unix.GetsockoptUcred(
		descriptor,
		unix.SOL_SOCKET,
		unix.SO_PEERCRED,
	)
	if err != nil {
		return 0, err
	}
	return credential.Uid, nil
}

func fileOwnerUID(info os.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("owner metadata is unavailable")
	}
	return stat.Uid, nil
}
