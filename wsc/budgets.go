package wsc

import (
	"errors"
	"time"
)

// Budgets 控制本机握手、每帧写入与客户端重连等待，构造时冻结。
// 零值沿用默认；心跳与读空闲仍遵守既有双方协议，不随本地写预算变化。
type Budgets struct {
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	ReconnectDelay   time.Duration
}

func (p Budgets) normalized() (Budgets, error) {
	if p.HandshakeTimeout < 0 || p.WriteTimeout < 0 || p.ReconnectDelay < 0 {
		return p, errors.New("wsc: budgets must be non-negative")
	}
	if p.HandshakeTimeout == 0 {
		p.HandshakeTimeout = HandshakeTimeout
	}
	if p.WriteTimeout == 0 {
		p.WriteTimeout = WriteTimeout
	}
	if p.ReconnectDelay == 0 {
		p.ReconnectDelay = 3 * time.Second
	}
	return p, nil
}

// WithBudgets 验证预算后返回 Client/Server 通用选项；单次 ConnectWithTimeout 仍优先。
func WithBudgets(p Budgets) (Option, error) {
	p, err := p.normalized()
	if err != nil {
		return nil, err
	}
	return func(o *options) { o.budgets = p }, nil
}

func (o options) resolvedBudgets() Budgets {
	p, _ := o.budgets.normalized()
	return p
}
