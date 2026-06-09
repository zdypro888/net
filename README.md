# github.com/zdypro888/net

通用网络工具库：HTTP/HTTP3 客户端、多路复用请求-响应客户端 (`Client`)、WebSocket 会话 (`wsc`)、WebSocket 代理 (`wsproxy`)、SOCKS5 等。

## ⚠️ 安全须知：TLS 默认不校验服务端证书

**本库的 HTTP/HTTP3 客户端默认 `InsecureSkipVerify: true`，并允许 TLS 1.1。**

出于历史与内网/抓包调试用途，`NewHTTP(nil)` / `NewHTTP3(nil)` 在 `config == nil` 时回退到 `DefaultTLSConfig()`：

```go
// http.go: DefaultTLSConfig()
&tls.Config{
    InsecureSkipVerify: true,            // 不校验服务端证书
    MinVersion:         tls.VersionTLS11, // 允许 TLS 1.1
    MaxVersion:         tls.VersionTLS13,
}
```

这意味着默认情况下：

- **不校验服务端证书链与主机名** —— 任何持有任意证书 (含自签名) 的中间人都能冒充目标服务端；
- **存在中间人攻击 (MITM) 风险**，在公网/不可信网络上传输敏感数据时尤其危险；
- 允许 TLS 1.1（已被主流浏览器废弃的旧协议版本）。

同样地，HTTPS 代理隧道 (`Proxy.TLSConfig`，见 `proxy.go`) 在该字段为 `nil` 时也回退到 `DefaultTLSConfig()`，即对代理服务端证书亦不校验。

> 该默认行为是刻意保留的历史契约，本说明仅作显式告警，不改变运行时默认值。

## 如何开启严格证书校验 (opt-in)

库已提供 `StrictTLSConfig()`，显式开启证书校验且最低 TLS 1.2：

```go
// http.go: StrictTLSConfig()
&tls.Config{
    MinVersion: tls.VersionTLS12,
    MaxVersion: tls.VersionTLS13,
}
```

### 1. HTTP / HTTP3 客户端

把 `StrictTLSConfig()`（或你自己的严格 `*tls.Config`）显式传给构造函数，**不要传 `nil`**：

```go
import (
    "github.com/zdypro888/net"
)

// 严格校验证书的 HTTP 客户端
h := net.NewHTTP(net.StrictTLSConfig())

// 严格校验证书的 HTTP3 (QUIC) 客户端
h3 := net.NewHTTP3(net.StrictTLSConfig())
```

需要自定义 (例如指定 CA 池 / ServerName) 时，直接传入构造好的 `*tls.Config`：

```go
h := net.NewHTTP(&tls.Config{
    MinVersion: tls.VersionTLS12,
    RootCAs:    myCertPool, // 自定义信任根
})
```

### 2. HTTPS 代理隧道

给 `Proxy.TLSConfig` 显式赋一个严格配置（用于 `https` scheme 代理的 CONNECT 隧道 TLS 握手）：

```go
proxy := &net.Proxy{
    Address:   "https://proxy.example.com:8443",
    TLSConfig: net.StrictTLSConfig(),
}
```

## 迁移建议

- **新接入方**：一律显式传 `net.StrictTLSConfig()`（或带自定义信任根的严格配置），不要依赖 `nil` 默认值。
- **存量代码**：审计所有 `NewHTTP(nil)` / `NewHTTP3(nil)` / 未设置 `Proxy.TLSConfig` 的调用点；除非确有内网/调试需要保留不校验，否则改为传入 `StrictTLSConfig()`。
- 若你的场景确实需要跳过校验（如内网自签名 + 无 CA 分发），请在调用点就近注释说明原因，避免被后续维护者误判为遗漏。
