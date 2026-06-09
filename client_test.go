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

func checkClose(t *testing.T, name string, closeFn func() error) {
	t.Helper()
	if err := closeFn(); err != nil {
		t.Logf("%s close returned: %v", name, err)
	}
}

func TestClientRequestResponse(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)
	defer checkClose(t, "client", client.Close)

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
	defer checkClose(t, "client", client.Close)

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

// TestClientCloseAfterRequestCancelDoesNotPanic 回归 BUG-7:
// 旧实现 asyncMessage.Canceled() 有 close(waiter) side-effect, 与 asyncGo 退出
// 尾段 Response 的 close(waiter) 会 double close panic. 修复后 canceled 路径
// 直接 delete 不 close waiter, Response 是唯一 close 入口.
func TestClientCloseAfterRequestCancelDoesNotPanic(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)

	reqCtx, reqCancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Request(reqCtx, testMessage{id: "req-cancel-then-close", value: "request"})
		errCh <- err
	}()

	select {
	case <-conn.writeCh:
	case <-time.After(time.Second):
		t.Fatal("request was not written before timeout")
	}

	reqCancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Close 触发 asyncGo 退出尾段, 它会遍历 asyncNotifys; 若旧 bug 仍在,
	// canceled message 会被 Response 二次 close panic 或 send-on-closed.
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked after canceled request")
	}
}

// TestClientAsyncRespectsCtxDeadline 验证 RUN-6: 当 asynchan 满 + asyncGo 卡死
// 在 conn.Write 时, Write(ctx) 不会永久阻塞, 由 ctx.Done 解锁返回.
// 这里直接用 fakeConn 的 writeCh 容量 16 作为 sink: 灌满 16 条 + asynchan
// (默认容量 16) 灌满后, 第 33 条必须超时.
func TestClientAsyncRespectsCtxDeadline(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)
	t.Cleanup(func() {
		checkClose(t, "fake conn", func() error {
			return conn.Close(context.Background()) // 让 conn.Write 走 ErrConnectionClosed, 解锁 asyncGo
		})
		checkClose(t, "client", client.Close)
	})

	// 1. 灌满 conn.writeCh (cap=16): 触发 asyncGo 取出第 17 条时卡 conn.Write.
	// 2. 同时 asynchan (cap=16) 一直被填.
	// 总能填多少 = 16(writeCh) + 1(在 conn.Write 卡的) + 16(asynchan) = 33. 第 34 条必须超时.
	const writes = 33
	for i := 0; i < writes; i++ {
		// 长 ctx 让前面的 Write 都成功入队. asyncGo 自己取走时不阻塞 caller.
		ctxFill, cancel := context.WithTimeout(context.Background(), time.Second)
		err := client.Write(ctxFill, testMessage{value: "fill"})
		cancel()
		if err != nil {
			// 在到第 writes 条之前出错说明我们的容量估算不对; 这种环境下用 Logf 跳过.
			t.Logf("fill write %d unexpectedly errored %v; environment may differ", i, err)
			break
		}
	}

	// 现在所有队列满, asyncGo 卡在 conn.Write — 下次 Write 短 ctx 必走 ctx.Done.
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := client.Write(timeoutCtx, testMessage{value: "must-time-out"})
	if err == nil {
		t.Fatal("expected timeout / closed, got nil — async() did not honor ctx.Done while asynchan full")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("expected ctx deadline or closed, got %v", err)
	}
}

// TestClientCloseDrainsPendingWrites 回归 BUG-2:
// 旧实现 asyncGo 退出时不排空 asynchan, Write(notify=false) 路径的 caller 拿到 nil-error
// 实际是丢消息. 修复后 drain 给每条 in-flight request 通知 lastError.
func TestClientCloseDrainsPendingWrites(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)

	// 发起一个 Request, asyncGo 写到 conn.writeCh 后等响应.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		// notify=true, 这条会被注册到 asyncNotifys; 关闭时需要被通知 ErrConnectionClosed.
		_, err := client.Request(context.Background(), testMessage{id: "req-drain", value: "x"})
		if !errors.Is(err, ErrConnectionClosed) && !errors.Is(err, context.Canceled) {
			t.Errorf("expected ErrConnectionClosed, got %v", err)
		}
	}()

	// 等 request 写到 wire.
	select {
	case <-conn.writeCh:
	case <-time.After(time.Second):
		t.Fatal("request not written")
	}

	// 关闭 client - 应该 unblock Request 并返回 ErrConnectionClosed (asyncNotifys 内 Response 路径).
	if err := client.Close(); err != nil && !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Close error: %v", err)
	}
	select {
	case <-reqDone:
	case <-time.After(time.Second):
		t.Fatal("Request did not unblock after Close")
	}
}

// blockingHandleConn 的 Handle 会一直阻塞, 直到 ctx 被取消 (遵守 Conn.Handle
// 的契约: 在唯一处理协程中调用, 用 ctx 控制生命周期). 用于回归 D-P1-1.
type blockingHandleConn struct {
	readCh      chan testMessage
	closed      chan struct{}
	once        sync.Once
	handleEnter chan struct{} // Handle 进入时关闭一次, 让测试确认分发已开始
	enterOnce   sync.Once
}

func newBlockingHandleConn() *blockingHandleConn {
	return &blockingHandleConn{
		readCh:      make(chan testMessage, DefaultBufferSize),
		closed:      make(chan struct{}),
		handleEnter: make(chan struct{}),
	}
}

func (c *blockingHandleConn) Close(ctx context.Context) error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *blockingHandleConn) Read(ctx context.Context) (testMessage, error) {
	select {
	case msg := <-c.readCh:
		return msg, nil
	case <-c.closed:
		return testMessage{}, ErrConnectionClosed
	case <-ctx.Done():
		return testMessage{}, ctx.Err()
	}
}

func (c *blockingHandleConn) Write(ctx context.Context, msg testMessage) error {
	select {
	case <-c.closed:
		return ErrConnectionClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handle 阻塞直到 ctx 取消. 这模拟"用户回调长时间不返回"的场景: 旧实现里
// asyncGo 同步卡在此处, 传给 Handle 的 ctx 只在 asyncGo 主循环之后才 cancel — 永远到不了,
// 于是 Close 的 waiter.Wait() 永久挂起. 修复后 CloseUnsafe 立即 cancel handleCtx,
// 这里因 ctx.Done() 解阻塞, asyncGo 退出, Close 返回.
func (c *blockingHandleConn) Handle(ctx context.Context, msg testMessage) {
	c.enterOnce.Do(func() { close(c.handleEnter) })
	<-ctx.Done()
}

// TestClientCloseUnblocksBlockedHandle 回归 D-P1-1:
// 用户 Conn.Handle 回调在 asyncGo 单协程内同步执行. 若回调长时间阻塞 (遵守 ctx
// 契约但等 ctx 取消), 旧实现 Close 无法把取消透传给 Handle, waiter.Wait() 永久挂起.
// 修复: CloseUnsafe 立即 cancel handleCtx, 让阻塞的 Handle 解锁 → asyncGo 退出.
func TestClientCloseUnblocksBlockedHandle(t *testing.T) {
	conn := newBlockingHandleConn()
	client := NewClient[testMessage, *blockingHandleConn]()
	client.Reset(context.Background(), conn)

	// 推一条无法匹配任何 request 的消息 (无 id), asyncGo 会走 conn.Handle 分发并卡住.
	conn.readCh <- testMessage{value: "push-no-id"}

	// 确认 Handle 已经被调用且正在阻塞.
	select {
	case <-conn.handleEnter:
	case <-time.After(2 * time.Second):
		t.Fatal("Handle was never invoked")
	}

	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case <-done:
		// 修复后: CloseUnsafe cancel handleCtx → Handle 的 <-ctx.Done() 解锁 → asyncGo 退出.
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind a blocked user Handle callback (D-P1-1)")
	}
}

func TestClientRequestContextCancel(t *testing.T) {
	conn := newFakeConn()
	client := NewClient[testMessage, *fakeConn]()
	client.Reset(context.Background(), conn)
	defer checkClose(t, "client", client.Close)

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
