package net

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type scopedTestTransport string

func (transport scopedTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(transport))), Request: request}, nil
}

// TestRequestTransportIsScoped 验证共享 HTTP 在并发请求中不会串用另一调用方的出口。
func TestRequestTransportIsScoped(t *testing.T) {
	client := NewHTTP(nil)
	defer client.Dispose()
	var group sync.WaitGroup
	for _, name := range []string{"local-a", "local-b"} {
		group.Go(func() {
			ctx := ContextWithTransport(context.Background(), scopedTestTransport(name))
			response, err := client.Request(ctx, "https://example.invalid", nil, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Close()
			body, err := io.ReadAll(response)
			if err != nil || string(body) != name {
				t.Errorf("request used wrong transport: %q %v", body, err)
			}
		})
	}
	group.Wait()
	if _, ok := client.client.Transport.(scopedTestTransport); ok {
		t.Fatal("request changed shared client transport")
	}
}
