// Package waitstate 将资源等待通知传回任务现有的日志或进度出口，不改变等待、重试和取消策略。
package waitstate

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}
type observer struct {
	mu     sync.Mutex
	next   uint64
	active map[uint64]string
	last   string
	report func(string)
}

// Observe 为一个任务安装并发安全的等待汇总器；相同原因合并，全部结束时报告空字符串。
// 回调同步且有序执行，应只更新任务进度，不可重入本观察器。
func Observe(ctx context.Context, report func(string)) context.Context {
	if report == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, &observer{active: map[uint64]string{}, report: report})
}

func (o *observer) notify() {
	unique := map[string]bool{}
	for _, text := range o.active {
		unique[text] = true
	}
	reasons := make([]string, 0, len(unique))
	for text := range unique {
		reasons = append(reasons, text)
	}
	sort.Strings(reasons)
	text := strings.Join(reasons, "；")
	if text != o.last {
		o.last = text
		o.report(text)
	}
}

// Observed 判断当前任务是否安装观察器，供独立调用入口沿用自身日志兜底。
func Observed(ctx context.Context) bool {
	_, ok := ctx.Value(contextKey{}).(*observer)
	return ok
}

// Begin 标记已经确认发生的资源等待，返回的结束函数可重复调用。
// message 必须是固定业务说明，不得传入含账号凭据、代理令牌或原始网络错误的内容。
func Begin(ctx context.Context, message string) func() {
	o, _ := ctx.Value(contextKey{}).(*observer)
	if o == nil || message == "" || ctx.Err() != nil {
		return func() {}
	}
	o.mu.Lock()
	o.next++
	id := o.next
	o.active[id] = message
	o.notify()
	o.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			delete(o.active, id)
			o.notify()
			o.mu.Unlock()
		})
	}
}

// After 只在调用持续超过阈值时报告等待，避免正常的快速取锁也刷出等待消息。
// 调用方必须 defer 返回函数；它会取消尚未触发的通知，并收回已发布的等待原因。
func After(ctx context.Context, delay time.Duration, message string) func() {
	if _, ok := ctx.Value(contextKey{}).(*observer); !ok {
		return func() {}
	}
	var mu sync.Mutex
	var clear func()
	ended := false
	timer := time.AfterFunc(delay, func() {
		mu.Lock()
		defer mu.Unlock()
		if !ended {
			clear = Begin(ctx, message)
		}
	})
	return func() {
		timer.Stop()
		mu.Lock()
		defer mu.Unlock()
		ended = true
		if clear != nil {
			clear()
			clear = nil
		}
	}
}
