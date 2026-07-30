//go:build !darwin && !linux

package peercred

import (
	"errors"
	"os"
)

func uidFromDescriptor(int) (uint32, error) {
	return 0, errors.New(
		"Unix peer credentials are supported only on Darwin and Linux",
	)
}

func fileOwnerUID(os.FileInfo) (uint32, error) {
	return 0, errors.New(
		"Unix ownership checks are supported only on Darwin and Linux",
	)
}
