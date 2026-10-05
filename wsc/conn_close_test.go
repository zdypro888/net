package wsc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Regression for the send/close race observed during real RPC shutdown. A full
// queue must release every concurrent sender and close without a receiver.
func TestConnectionConcurrentCloseReleasesFullQueue(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	for range 50 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+server.URL[len("http"):], nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		ws := createWSConnection[testPayload](conn, 1, defaultCodec)
		ws.Handle(ctx, &Message[testPayload]{Data: testPayload{Kind: "queued", Value: 7}})
		var workers sync.WaitGroup
		ready, start := make(chan struct{}, 64), make(chan struct{})
		for range 64 {
			workers.Go(func() {
				ready <- struct{}{}
				<-start
				ws.Handle(ctx, &Message[testPayload]{Data: testPayload{Kind: "blocked"}})
			})
		}
		for range 64 {
			<-ready
		}
		close(start)
		done := make(chan error, 1)
		go func() { err := ws.Close(context.Background()); workers.Wait(); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				cancel()
				t.Fatal(err)
			}
		case <-ctx.Done():
			cancel()
			t.Fatal("close failed to release senders under backpressure")
		}
		// Queued messages are retained, then EOF; a post-close handler cannot reopen
		// the stream or panic. Concurrent Close callers observe the same result.
		packet := <-ws.channel()
		if packet == nil || packet.Message.Data.Value != 7 {
			cancel()
			t.Fatal("queued message lost")
		}
		if _, ok := <-ws.channel(); ok {
			cancel()
			t.Fatal("message channel not closed")
		}
		ws.Handle(ctx, &Message[testPayload]{Data: testPayload{Kind: "after-close"}})
		if err := ws.Close(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
	}
}
