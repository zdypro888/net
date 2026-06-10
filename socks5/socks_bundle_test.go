package socks5

import (
	"bytes"
	"context"
	"io"
	"testing"
)

type socksAuthReadWriter struct {
	writes bytes.Buffer
	reply  []byte
}

func (rw *socksAuthReadWriter) Write(p []byte) (int, error) {
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
}
