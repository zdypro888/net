package wsc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testPayload struct {
	Kind  string `json:"kind"`
	Value int    `json:"value"`
}

func TestClientServerWriteAndRequest(t *testing.T) {
	server := NewServerWithBuffer[testPayload](64)
	upgrader := websocket.Upgrader{}
	sessionCh := make(chan *Session[testPayload], 1)
	errCh := make(chan error, 1)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		session, err := server.OnConnection(conn, nil)
		if err != nil {
			errCh <- err
			return
		}
		sessionCh <- session
	}))
	defer httpServer.Close()
	defer server.Close()

	client := NewClientWithBuffer[testPayload]("ws"+httpServer.URL[len("http"):], 64)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	var session *Session[testPayload]
	select {
	case session = <-sessionCh:
	case err := <-errCh:
		t.Fatalf("server connection failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for session: %v", ctx.Err())
	}

	if err := client.Write(ctx, testPayload{Kind: "notify", Value: 7}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	select {
	case packet := <-session.Handle():
		if packet.ID != "" {
			t.Fatalf("notification should not have request id: %q", packet.ID)
		}
		if packet.Data.Kind != "notify" || packet.Data.Value != 7 {
			t.Fatalf("unexpected notification payload: %#v", packet.Data)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for notification: %v", ctx.Err())
	}

	replyDone := make(chan struct{})
	go func() {
		defer close(replyDone)
		packet := <-session.Handle()
		if packet.ID == "" {
			t.Errorf("request missing id")
			return
		}
		if err := session.Reply(context.Background(), packet.ID, testPayload{Kind: "reply", Value: packet.Data.Value + 1}); err != nil {
			t.Errorf("Reply failed: %v", err)
		}
	}()

	resp, err := client.Request(ctx, testPayload{Kind: "request", Value: 41})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if resp.Kind != "reply" || resp.Value != 42 {
		t.Fatalf("unexpected response: %#v", resp)
	}
	select {
	case <-replyDone:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for reply goroutine: %v", ctx.Err())
	}
}
