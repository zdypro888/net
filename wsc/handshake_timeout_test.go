package wsc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandshakePolicyValidationAndCancellation(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ctx, err := WithHandshakeTimeout(parent, time.Hour)
	if err != nil || handshakeTimeout(ctx) != time.Hour {
		t.Fatal("override lost", err)
	}
	deadline, _ := ctx.Deadline()
	original, _ := parent.Deadline()
	if !deadline.Equal(original) {
		t.Fatal("parent budget changed")
	}
	for _, bad := range []time.Duration{0, -1} {
		if _, err := WithHandshakeTimeout(parent, bad); err == nil {
			t.Fatal("invalid budget accepted")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := NewClient[string]("ws" + strings.TrimPrefix(server.URL, "http"))
	defer client.Close()
	ctx, err = WithHandshakeTimeout(t.Context(), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := client.Connect(ctx); err == nil {
		t.Fatal("blocked Upgrade ignored policy")
	}
	if time.Since(start) > time.Second {
		t.Fatal("fixed handshake default overrode task budget")
	}
}
