package peercred

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestUIDAuthenticatesBothEndsOfUnixConnection(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: socketPath, Net: "unix"},
	)
	if err != nil {
		t.Skipf("Unix sockets are unavailable: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan *net.UnixConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()
	client, err := net.DialUnix(
		"unix",
		nil,
		&net.UnixAddr{Name: socketPath, Net: "unix"},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var server *net.UnixConn
	select {
	case server = <-accepted:
		t.Cleanup(func() { _ = server.Close() })
	case err := <-acceptErr:
		t.Fatal(err)
	}

	for name, connection := range map[string]net.Conn{
		"server observes client": server,
		"client observes server": client,
	} {
		t.Run(name, func(t *testing.T) {
			uid, err := UID(connection)
			if err != nil {
				t.Fatal(err)
			}
			if uid != uint32(os.Geteuid()) {
				t.Fatalf("peer UID=%d, want %d", uid, os.Geteuid())
			}
		})
	}
}

func TestUIDRejectsTransportWithoutUnixDescriptor(t *testing.T) {
	first, second := net.Pipe()
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})
	if _, err := UID(first); err == nil {
		t.Fatal("non-Unix transport produced peer credentials")
	}
}
