package net

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type testMessage struct {
	id    string
	value string
}

func (m testMessage) Id() (any, bool) {
	if m.id == "" {
		return nil, false
	}
	return m.id, true
}

type fakeConn struct {
	readCh  chan testMessage
	writeCh chan testMessage
	closed  chan struct{}
	once    sync.Once

	mu      sync.Mutex
	handled []testMessage
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		readCh:  make(chan testMessage, DefaultBufferSize),
		writeCh: make(chan testMessage, DefaultBufferSize),
		closed:  make(chan struct{}),
	}
}

func (c *fakeConn) Close(ctx context.Context) error {
	c.once.Do(func() {
		close(c.closed)
	})
	return nil
}

func (c *fakeConn) Read(ctx context.Context) (testMessage, error) {
	select {
	case msg := <-c.readCh:
		return msg, nil
	case <-c.closed:
		return testMessage{}, ErrConnectionClosed
	case <-ctx.Done():
		return testMessage{}, ctx.Err()
	}
}

func (c *fakeConn) Write(ctx context.Context, msg testMessage) error {
	select {
	case c.writeCh <- msg:
		return nil
	case <-c.closed:
		return ErrConnectionClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeConn) Handle(ctx context.Context, msg testMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handled = append(c.handled, msg)
}

func TestClientRequestResponse(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := <-conn.writeCh
		if req.id != "req-1" || req.value != "request" {
			t.Errorf("unexpected request: %#v", req)
			return
		}
		conn.readCh <- testMessage{id: req.id, value: "response"}
	}()

	resp, err := client.Request(ctx, testMessage{id: "req-1", value: "request"})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if resp.id != "req-1" || resp.value != "response" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	<-done
}

func TestClientRequestReturnsWhenConnectionCloses(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := client.Request(ctx, testMessage{id: "req-close", value: "request"})
		errCh <- err
	}()

	select {
	case <-conn.writeCh:
	case <-ctx.Done():
		t.Fatalf("request was not written before timeout: %v", ctx.Err())
	}

	if err := conn.Close(context.Background()); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrConnectionClosed) {
			t.Fatalf("expected ErrConnectionClosed, got %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("request did not return after connection close: %v", ctx.Err())
	}
}

func TestClientRequestContextCancel(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Request(ctx, testMessage{id: "req-cancel", value: "request"})
		errCh <- err
	}()

	select {
	case <-conn.writeCh:
	case <-time.After(time.Second):
		t.Fatal("request was not written before timeout")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not return after context cancellation")
	}

	conn.readCh <- testMessage{id: "req-cancel", value: "late-response"}
}
