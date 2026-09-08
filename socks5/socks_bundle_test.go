package socks5

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type socksAuthReadWriter struct {
	writes     bytes.Buffer
	reply      []byte
	writeLimit int
}

func (rw *socksAuthReadWriter) Write(p []byte) (int, error) {
	if rw.writeLimit > 0 && len(p) > rw.writeLimit {
		p = p[:rw.writeLimit]
	}
	return rw.writes.Write(p)
}

func (rw *socksAuthReadWriter) Read(p []byte) (int, error) {
	if len(rw.reply) == 0 {
		return 0, io.EOF
	}
	n := copy(p, rw.reply)
	rw.reply = rw.reply[n:]
	return n, nil
}

func TestUsernamePasswordAuthenticateAllowsEmptyPasswordLikeUpstream(t *testing.T) {
	rw := &socksAuthReadWriter{
		reply: []byte{socksauthUsernamePasswordVersion, socksauthStatusSucceeded},
	}
	auth := &UsernamePassword{Username: "user"}
	if err := auth.Authenticate(context.Background(), rw, AuthMethodUsernamePassword); err != nil {
		t.Fatalf("Authenticate with empty password failed: %v", err)
	}
	want := []byte{
		socksauthUsernamePasswordVersion,
		byte(len(auth.Username)),
		'u', 's', 'e', 'r',
		0,
	}
	if got := rw.writes.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("auth frame = %v, want %v", got, want)
	}
	rw.writeLimit = 1
	if err := auth.Authenticate(context.Background(), rw, AuthMethodUsernamePassword); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short auth write=%v", err)
	}
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() {
		defer peer.Close()
		_, _ = io.ReadFull(peer, make([]byte, 3))
		_, _ = peer.Write([]byte{socksVersion5, byte(AuthMethodUsernamePassword)})
	}()
	dialer := NewDialer("tcp", "proxy.invalid:1080")
	if _, err := dialer.DialWithConn(context.Background(), client, "tcp", "target.invalid:443"); err == nil || !strings.Contains(err.Error(), "unoffered") {
		t.Fatalf("unoffered authentication was not rejected: %v", err)
	}
	client2, peer2 := net.Pipe()
	defer client2.Close()
	defer peer2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := dialer.DialWithConn(ctx, client2, "tcp", "target.invalid:443"); done <- err }()
	if _, err := io.ReadFull(peer2, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handshake cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS handshake ignored cancellation")
	}

}
