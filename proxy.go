package net

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/zdypro888/net/socks5"
	"github.com/zdypro888/net/wsproxy"
	"github.com/zdypro888/utils"
)

const proxyConnectTimeout = 30 * time.Second

// HTTPDebugProxy 调试代理
var HTTPDebugProxy = &Proxy{Address: "http://127.0.0.1:8888"}

// Proxy 代理
type Proxy struct {
	Address string `bson:"Address" json:"Address"`
	WSToken string `bson:"WSToken,omitempty" json:"WSToken,omitempty"`
	// TLSConfig 仅用于 https scheme proxy 的 CONNECT 隧道 TLS 握手.
	// nil 使用 DefaultTLSConfig; 需要校验证书时显式传 StrictTLSConfig().
	TLSConfig *tls.Config     `bson:"-" json:"-"`
	server    *wsproxy.Server `bson:"-" json:"-"`
}

// resolve 把 Address (可能含模板占位符) 渲染并解析为 url.URL.
// 单点入口避免 ProxyURL / DialContext 各做一次 RandomTemplateText + url.Parse
// 时实现漂移. 不缓存: Address 可能含每次需重渲染的占位符.
func (proxy *Proxy) resolve() (*url.URL, error) {
	address, err := utils.RandomTemplateText(proxy.Address)
	if err != nil {
		return nil, err
	}
	return url.Parse(address)
}

// ProxyURL 取得代理地址 (实现 http.Transport.Proxy 的签名).
func (proxy *Proxy) ProxyURL(req *http.Request) (*url.URL, error) {
	return proxy.resolve()
}

// Dial 拨号
func (proxy *Proxy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	proxyURL, err := proxy.resolve()
	if err != nil {
		return nil, err
	}
	switch proxyURL.Scheme {
	case "socks5":
		d := socks5.NewDialer("tcp", proxyURL.Host)
		if proxyURL.User != nil {
			auth := &socks5.UsernamePassword{
				Username: proxyURL.User.Username(),
			}
			if password, ok := proxyURL.User.Password(); ok {
				auth.Password = password
			}
			d.AuthMethods = []socks5.AuthMethod{
				socks5.AuthMethodNotRequired,
				socks5.AuthMethodUsernamePassword,
			}
			d.Authenticate = auth.Authenticate
		}
		return d.DialContext(ctx, network, address)
	case "http", "https":
		dialer := &net.Dialer{}
		conn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
		if err != nil {
			return nil, err
		}
		// rawConn 固定指向底层 TCP 连接。下面 https 分支会把 conn 重新指向 tlsConn,
		// 而本 watcher 在另一 goroutine 读取被捕获的连接; 若直接捕获 conn 变量, ctx 在
		// 握手成功后瞬间取消时, watcher 读 conn 会与主 goroutine 的 `conn = tlsConn` 写
		// 并发 → net.Conn 接口值(双字)撕裂读。关闭底层 TCP 已足以中断 TLS 握手/CONNECT。
		rawConn := conn
		stopContextClose := context.AfterFunc(ctx, func() {
			if err := rawConn.Close(); err != nil {
				slog.Debug("net.Proxy context close connection failed", slog.Any("err", err))
			}
		})
		defer stopContextClose()
		closeWithContextError := func(err error) error {
			closeErr := conn.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return errors.Join(ctxErr, closeErr)
			}
			return errors.Join(err, closeErr)
		}
		if proxyURL.Scheme == "https" {
			host := proxyURL.Hostname()
			tlsCfg := proxy.TLSConfig
			if tlsCfg == nil {
				tlsCfg = DefaultTLSConfig()
			}
			// Clone 以避免污染 caller 持有的 *tls.Config (ServerName 会被本次填入).
			tlsCfg = tlsCfg.Clone()
			if tlsCfg.ServerName == "" {
				tlsCfg.ServerName = host
			}
			tlsConn := tls.Client(conn, tlsCfg)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				return nil, closeWithContextError(err)
			}
			conn = tlsConn
		}
		deadline := time.Now().Add(proxyConnectTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok {
			deadline = ctxDeadline
		}
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, closeWithContextError(err)
		}
		connectReq := (&http.Request{
			Method: "CONNECT",
			URL:    &url.URL{Opaque: address},
			Host:   address,
			Header: make(http.Header),
		}).WithContext(ctx)
		if proxyURL.User != nil {
			auth := proxyURL.User.Username() + ":"
			if password, ok := proxyURL.User.Password(); ok {
				auth += password
			}
			connectReq.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
		}
		if err = connectReq.Write(conn); err != nil {
			return nil, closeWithContextError(err)
		}
		br := bufio.NewReader(conn)
		var response *http.Response
		if response, err = http.ReadResponse(br, connectReq); err != nil {
			return nil, closeWithContextError(err)
		}
		if response.StatusCode != 200 {
			return nil, closeWithContextError(fmt.Errorf("connect http tunnel faild: %d", response.StatusCode))
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return nil, closeWithContextError(err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(ctxErr, conn.Close())
		}
		if !stopContextClose() {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, errors.Join(ctxErr, conn.Close())
			}
		}
		return conn, nil
	case "ws", "wss":
		if proxy.server != nil {
			return proxy.server.DialContext(ctx, network, address)
		} else {
			client := wsproxy.NewClient(proxyURL.String())
			client.Token = proxy.WSToken
			return client.Dial(ctx, network, address)
		}
	}
	return nil, fmt.Errorf("type: %s not supported", proxyURL.Scheme)
}

func (proxy *Proxy) WithWSServer(server *wsproxy.Server) {
	proxy.server = server
}
