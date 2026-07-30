package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/duriantaco/vouch/internal/kernel/peercred"
)

type contextDialer func(
	context.Context,
	string,
	string,
) (net.Conn, error)

func dialVerifiedUnix(
	ctx context.Context,
	dial contextDialer,
	socketPath string,
) (net.Conn, error) {
	return dialVerifiedUnixForUID(
		ctx,
		dial,
		socketPath,
		uint32(os.Geteuid()),
	)
}

func dialVerifiedUnixForUID(
	ctx context.Context,
	dial contextDialer,
	socketPath string,
	clientUID uint32,
) (net.Conn, error) {
	if dial == nil {
		return nil, errors.New("connect to gatemoled: Unix dialer is required")
	}
	if strings.TrimSpace(socketPath) == "" ||
		strings.TrimSpace(socketPath) != socketPath {
		return nil, errors.New(
			"connect to gatemoled: Unix socket path is required without surrounding whitespace",
		)
	}
	absolutePath, err := filepath.Abs(socketPath)
	if err != nil {
		return nil, fmt.Errorf(
			"connect to gatemoled: resolve Unix socket path: %w",
			err,
		)
	}
	beforeSocket, beforeParent, ownerUID, err :=
		inspectUnixSocket(absolutePath)
	if err != nil {
		return nil, err
	}
	if ownerUID != clientUID {
		return nil, fmt.Errorf(
			"connect to gatemoled: Unix socket owner UID %d does not match client effective UID %d",
			ownerUID,
			clientUID,
		)
	}

	connection, err := dial(ctx, "unix", absolutePath)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = connection.Close()
		}
	}()

	peerUID, err := peercred.UID(connection)
	if err != nil {
		return nil, fmt.Errorf(
			"connect to gatemoled: authenticate Unix peer: %w",
			err,
		)
	}
	afterSocket, afterParent, afterOwnerUID, err :=
		inspectUnixSocket(absolutePath)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(beforeSocket, afterSocket) ||
		!os.SameFile(beforeParent, afterParent) ||
		ownerUID != afterOwnerUID {
		return nil, errors.New(
			"connect to gatemoled: Unix socket changed while connecting",
		)
	}
	if peerUID != ownerUID {
		return nil, fmt.Errorf(
			"connect to gatemoled: Unix peer UID %d does not own socket UID %d",
			peerUID,
			ownerUID,
		)
	}

	closeOnError = false
	return connection, nil
}

func inspectUnixSocket(
	socketPath string,
) (os.FileInfo, os.FileInfo, uint32, error) {
	parentPath := filepath.Dir(socketPath)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf(
			"connect to gatemoled: inspect Unix socket directory: %w",
			err,
		)
	}
	if !parentInfo.IsDir() ||
		parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, 0, errors.New(
			"connect to gatemoled: Unix socket directory must be a real directory",
		)
	}
	if parentInfo.Mode().Perm()&0o022 != 0 {
		return nil, nil, 0, errors.New(
			"connect to gatemoled: Unix socket directory must not be group- or world-writable",
		)
	}

	socketInfo, err := os.Lstat(socketPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf(
			"connect to gatemoled: inspect Unix socket: %w",
			err,
		)
	}
	if socketInfo.Mode()&os.ModeSymlink != 0 ||
		socketInfo.Mode()&os.ModeSocket == 0 {
		return nil, nil, 0, errors.New(
			"connect to gatemoled: path must be a real Unix socket",
		)
	}
	if socketInfo.Mode().Perm()&0o077 != 0 {
		return nil, nil, 0, errors.New(
			"connect to gatemoled: Unix socket must not grant group or other access",
		)
	}
	ownerUID, err := peercred.FileOwnerUID(socketInfo)
	if err != nil {
		return nil, nil, 0, fmt.Errorf(
			"connect to gatemoled: inspect Unix socket owner: %w",
			err,
		)
	}
	return socketInfo, parentInfo, ownerUID, nil
}
