package waitstate

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConcurrentReasonsKeepRemainingWait(t *testing.T) {
	var got []string
	ctx := Observe(t.Context(), func(text string) { got = append(got, text) })
	a := Begin(ctx, "等待代理")
	b := Begin(ctx, "等待代理")
	c := Begin(ctx, "等待设备")
	a()
	a()
	b()
	c()
	want := []string{"等待代理", "等待代理；等待设备", "等待设备", ""}
	if len(got) != len(want) {
		t.Fatalf("events=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events=%v", got)
		}
	}
}

func TestAfterDoesNotFlashOnFastPathAndClearsSlowWait(t *testing.T) {
	events := make(chan string, 8)
	ctx := Observe(t.Context(), func(text string) { events <- text })
	finish := After(ctx, time.Hour, "等待资源")
	finish()
	finish()
	select {
	case text := <-events:
		t.Fatalf("fast operation emitted %q", text)
	default:
	}
	finish = After(ctx, time.Millisecond, "等待资源")
	select {
	case text := <-events:
		if text != "等待资源" {
			t.Fatal(text)
		}
	case <-time.After(time.Second):
		t.Fatal("missing wait")
	}
	finish()
	if text := <-events; text != "" {
		t.Fatal(text)
	}
}

func TestConcurrentWaitsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	events := 0
	ctx = Observe(ctx, func(string) { events++ })
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() { done := Begin(ctx, "等待资源"); defer done() })
	}
	workers.Wait()
	cancel()
	Begin(ctx, "不应发布")()
	// 全部通知结束后，无尚未撤回的资源状态；race 检查回调和去重的并发边界。
	if events == 0 || events%2 != 0 {
		t.Fatalf("unbalanced transitions=%d", events)
	}
}
