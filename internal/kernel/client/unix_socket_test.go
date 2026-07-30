package client

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDialVerifiedUnixAuthenticatesStablePrivateSocket(t *testing.T) {
	socketPath, listener := listenPrivateUnixSocket(t)
	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			err = connection.Close()
		}
		accepted <- err
	}()

	dialer := &net.Dialer{Timeout: time.Second}
	connection, err := dialVerifiedUnix(
		context.Background(),
		dialer.DialContext,
		socketPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestDialVerifiedUnixRejectsUnsafePathBeforeDial(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"permissive socket": func(t *testing.T, socketPath string) {
			if err := os.Chmod(socketPath, 0o660); err != nil {
				t.Fatal(err)
			}
		},
		"permissive directory": func(t *testing.T, socketPath string) {
			if err := os.Chmod(filepath.Dir(socketPath), 0o770); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			socketPath, _ := listenPrivateUnixSocket(t)
			mutate(t, socketPath)
			dialCalled := false
			_, err := dialVerifiedUnix(
				context.Background(),
				func(
					context.Context,
					string,
					string,
				) (net.Conn, error) {
					dialCalled = true
					return nil, nil
				},
				socketPath,
			)
			if err == nil || dialCalled {
				t.Fatalf(
					"unsafe Unix path reached dial: called=%t err=%v",
					dialCalled,
					err,
				)
			}
		})
	}
}

func TestDialVerifiedUnixRejectsSocketNotOwnedByClientBeforeDial(
	t *testing.T,
) {
	socketPath, _ := listenPrivateUnixSocket(t)
	dialCalled := false
	otherUID := uint32(os.Geteuid()) ^ 1

	_, err := dialVerifiedUnixForUID(
		context.Background(),
		func(
			context.Context,
			string,
			string,
		) (net.Conn, error) {
			dialCalled = true
			return nil, nil
		},
		socketPath,
		otherUID,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "does not match client effective UID") ||
		dialCalled {
		t.Fatalf(
			"foreign-owned Unix socket reached dial: called=%t err=%v",
			dialCalled,
			err,
		)
	}
}

func TestDialVerifiedUnixRejectsSocketReplacement(t *testing.T) {
	socketPath, original := listenPrivateUnixSocket(t)
	replacementDirectory := filepath.Dir(socketPath)
	var replacement *net.UnixListener
	dialer := &net.Dialer{Timeout: time.Second}

	_, err := dialVerifiedUnix(
		context.Background(),
		func(
			ctx context.Context,
			network string,
			address string,
		) (net.Conn, error) {
			original.SetUnlinkOnClose(false)
			if err := original.Close(); err != nil {
				return nil, err
			}
			if err := os.Remove(address); err != nil {
				return nil, err
			}
			var listenErr error
			replacement, listenErr = net.ListenUnix(
				"unix",
				&net.UnixAddr{Name: address, Net: "unix"},
			)
			if listenErr != nil {
				return nil, listenErr
			}
			replacement.SetUnlinkOnClose(false)
			if chmodErr := os.Chmod(address, 0o600); chmodErr != nil {
				return nil, chmodErr
			}
			return dialer.DialContext(ctx, network, address)
		},
		socketPath,
	)
	if replacement != nil {
		_ = replacement.Close()
		_ = os.Remove(filepath.Join(
			replacementDirectory,
			filepath.Base(socketPath),
		))
	}
	if err == nil ||
		!strings.Contains(err.Error(), "changed while connecting") {
		t.Fatalf("replaced Unix socket was accepted: %v", err)
	}
}

func listenPrivateUnixSocket(
	t *testing.T,
) (string, *net.UnixListener) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "vouchd.sock")
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: socketPath, Net: "unix"},
	)
	if err != nil {
		t.Skipf("Unix sockets are unavailable: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})
	return socketPath, listener
}
