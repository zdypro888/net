package net

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
)

// NetworkEvent 只记录出口诊断元数据，不含请求路径、查询参数、头、正文或原始错误。
// DurationMS 为收到响应头/发送失败的耗时，不包含响应正文下载。
type NetworkEvent struct {
	EndpointID string `json:"endpoint_id"`
	Operation  string `json:"operation,omitempty"`
	Step       string `json:"step,omitempty"`
	Method     string `json:"method"`
	Host       string `json:"host"`
	Match      string `json:"match"`
	Mode       string `json:"mode"`
	Exit       string `json:"exit"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}
type networkObserverKey struct{}
type networkObserver struct {
	report func(NetworkEvent)
}

// ContextWithNetworkObserver 安装任务级观察器；回调必须线程安全且不执行网络等待。
func ContextWithNetworkObserver(ctx context.Context, report func(NetworkEvent)) context.Context {
	if report == nil {
		return ctx
	}
	return context.WithValue(ctx, networkObserverKey{}, &networkObserver{report: report})
}

// NetworkRouteLabel 只展示代理协议和地址；不包含用户密码、路径或查询令牌。
func NetworkRouteLabel(route NetworkRoute) string {
	if route.Proxy == nil {
		return "direct"
	}
	u, err := url.Parse(route.Proxy.Address)
	if err != nil || u.Host == "" {
		return "proxy"
	}
	return u.Scheme + "://" + u.Host
}

// endpointID 允许关联未命名接口，但不输出可能含用户标识的路径。
func endpointID(u *url.URL) string {
	sum := sha256.Sum256([]byte(requestEndpoint(u)))
	return hex.EncodeToString(sum[:6])
}
