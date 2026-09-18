package net

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stalledPushConn struct {
	*fakeConn
	entered chan struct{}
}

func (c *stalledPushConn) Handle(ctx context.Context, _ testMessage) { close(c.entered); <-ctx.Done() }

// 推送回调阻塞期间，请求仍应写出并匹配响应，避免 APN actor 与 net 队列互等。
func TestPushHandlerDoesNotBlockResponse(t *testing.T) {
	c := &stalledPushConn{newFakeConn(), make(chan struct{})}
	client := NewClient[testMessage, *stalledPushConn]()
	client.Reset(context.Background(), c)
	defer client.Close()
	c.readCh <- testMessage{value: "push"}
	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	go func() {
		select {
		case req := <-c.writeCh:
			c.readCh <- testMessage{id: req.id, value: "ok"}
		case <-c.closed:
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := client.Request(ctx, testMessage{id: "request"})
	if err != nil || got.value != "ok" {
		t.Fatalf("response blocked: %v %v", got, err)
	}
}
func TestPushOverloadClosesAndCancelsHandler(t *testing.T) {
	c := &stalledPushConn{newFakeConn(), make(chan struct{})}
	client := NewClient[testMessage, *stalledPushConn]()
	client.SetBufferSize(1)
	client.Reset(context.Background(), c)
	defer client.Close()
	c.readCh <- testMessage{value: "first"}
	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	c.readCh <- testMessage{value: "queued"}
	c.readCh <- testMessage{value: "overflow"}
	select {
	case <-c.closed:
	case <-time.After(time.Second):
		t.Fatal("overload hung")
	}
	if err := client.Close(); !errors.Is(err, ErrPushQueueFull) {
		t.Fatalf("unexpected error: %v", err)
	}
}
