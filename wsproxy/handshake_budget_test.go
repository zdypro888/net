package wsproxy

import (
	"context"
	"testing"
	"time"
)

func TestExplicitHandshakeBudgetIsNotCappedByDefault(t *testing.T) {
	ctx, err := WithHandshakeTimeout(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(handshakeDeadline(ctx)); remaining < 50*time.Second {
		t.Fatal("explicit budget cut by old default", remaining)
	}
	parent, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	deadline, _ := parent.Deadline()
	if handshakeDeadline(parent) != deadline {
		t.Fatal("budget extended parent deadline")
	}
}
