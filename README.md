# github.com/zdypro888/net

通用网络库：HTTP/HTTP2/HTTP3、请求关联客户端 `Client`、可重连 WebSocket 会话 `wsc`、反向代理 `wsproxy`、SOCKS5 和可持久化 Cookie Jar。

## HTTP 与证书信任

`NewHTTP(nil)`、`NewHTTP3(nil)`、`DefaultTLSConfig()` 和 HTTPS 代理的 nil TLS 配置均默认验证证书链与主机名，最低 TLS 1.2。QUIC 自身使用 TLS 1.3。

```go
h := net.NewHTTP(nil)
defer h.Dispose()
if err := h.ConfigureV2(); err != nil {
    return err
}
res, err := h.Request(ctx, endpoint, headers, nil)
if err != nil {
    return err
}
data, err := res.Data() // 读取并关闭响应
```

私有 CA 或抓包代理应配置系统信任根，或显式传入带 `RootCAs` 的 `tls.Config`。HTTPS 代理的信任配置使用 `Proxy.TLSConfig`，目标服务器的信任配置使用 `NewHTTP` 的参数。`ConfigureDebug()` 只切换代理，不关闭证书校验。确有受控测试需求时，调用方仍可显式传入 `InsecureSkipVerify: true`，需自行承担该配置的影响。

迁移注意：旧版本 nil 配置跳过证书校验且允许 TLS 1.1。依赖旧默认值的自签名服务器、HTTPS 代理或 TLS 1.1 服务需要调整信任/协议配置，不能依赖隐式放行。

## 配置快照与连接池

`Configure*` 方法可以与请求并发。每次请求取得独立配置快照；修改代理、HTTP/2 或 Transport 超时，以及调用 `ResetConnections()` 时，会替换连接池。已经发出的请求继续使用原配置，后续请求不会复用旧代理的连接。HTTP/1 空闲连接有回收期限。

`ConfigureProxyClear()` 暂停代理，`ConfigureProxyReset()` 恢复 `storeCache=true` 保存的配置。HTTP/3 不支持 TCP 代理配置，返回错误的方法不会部分写入缓存。

`OnResponse` / `AutoRetry` 导出字段仅为兼容旧调用保留；直接赋值必须在请求开始前完成。运行期间请用 `ConfigureOnResponse` / `ConfigureAutoRetry`。传入的 CookieJar、回调及拨号函数必须并发安全；`Proxy` 自身的导出字段应在使用前配置完成。

`OnResponse` 返回错误或丢弃响应时，库关闭该次响应；不能返回 `(nil, nil, ...)`。若成功返回替代响应，回调负责转交或释放原响应 Body 的所有权，避免丢失原 Body。返回值中的 bool 用于关闭空闲连接。

## HTTP 重试和响应读取

- `ConfigureAutoRetry(n)` 保留历史计数含义：正数为**最多尝试次数，包含首次**；`n <= 0` 为一次。
- 仅自动重发可重放的幂等请求。POST 等非幂等请求默认只发送一次；明确携带 `Idempotency-Key` / `X-Idempotency-Key` 的请求可重试，调用方须确认服务端支持该契约。
- `bytes.Reader`、`strings.Reader`、`bytes.Buffer` 和 `utils.Reader` 从传入时的当前位置发送，每次重试使用独立 body。没有安全重放能力的普通流只发送一次，不会无界缓冲或强行 seek。
- 不自动重试 HTTP 状态码；`OnResponse` 可将状态转换成错误，仍须遵守上述重发限制。业务请求的结果不明确时，由业务方查询或判断后决定是否重试。
- `Response.Read/Data` 支持 gzip、zlib 格式 deflate、Brotli、Zstandard 和叠加编码。保留原响应头；直接访问嵌入的 `Body` 仍取得原始流。
- `Data()` 负责关闭；流式读取后必须 `Close()`。`Close` 可与 `Read` 并发。不要复制使用中的 `HTTP` 或 `Response` 值。

## Client / wsc 生命周期

`Client` 的状态锁只用于取得队列快照，满队列不会挡住 `Close/Reset`。关闭先发停止/取消信号，再关闭 IO 和排空待处理请求；旧连接的响应不会跨代匹配。取消的请求 ID 保留到迟到响应被消耗，或连接结束，不能立即复用。

`ResetUnsafe/CloseUnsafe` 仅供独占生命周期的单 owner 调用。其它带 Unsafe 的旧方法名保留，但入队使用与普通方法相同的同步门控。`Write` 成功表示已入队，ctx 仅控制入队；它不代表对端已收到数据。需要确定结果时使用请求/响应协议。

`Conn.Handle` 和 `AsyncCall` 回调须响应传入 ctx；不得在自身 worker 中同步调用 `Close/Reset/Request`。回调忽略取消并永久阻塞时，库无法强制结束该 goroutine。

`wsc.Client.Close` 会取消当前 Connect 握手。Session 关闭中止排队的连接切换，旧代清理定时器不能关闭重接后的会话。`ReplyGeneration` 用于只回复收到请求的连接代次；`TakeConnectionArgs` 每个非关闭 Packet 领取一次。应用必须持续消费 `Handle()`，并在不用时关闭 Client/Server/Session。

## 代理

`Proxy.DialContext` 支持 HTTP CONNECT、HTTPS CONNECT、SOCKS5/SOCKS5h、WS/WSS 字节流代理，目标 network 为 tcp/tcp4/tcp6。HTTP(S)/SOCKS5 省略端口时分别采用协议默认端口；SOCKS5/SOCKS5h 均由代理解析目标域名。

`wsproxy.Server` 的待命池、正在拨号、已返回的隧道及尚未收到首包的连接均纳入 `CloseAll`。待命 Ping/Pong 的 deadline 在转交活动隧道时清除。`Slaver.Run` 取消后等待已派发隧道退出。

公开服务应配置 `Token` 或 `ScopeByToken + AuthorizeToken`，或由 HTTP Upgrade 入口完成鉴权；空 Token 的默认模式不提供应用层认证。业务租户隔离须由调用方提供，不能把随机 session GUID 当成鉴权凭证。

WebSocket 代理按流分块转发，不为大帧分配完整副本；会保留 Read 同时返回的数据与 EOF。`Session` 支持 deadline，但 Gorilla 在**帧读取/写入途中超时后不能恢复该方向**，应重新拨号；尚未开始的过期写不会进入 Gorilla。此限制不同于普通 TCP，不能把超时后的 WS 隧道继续当成可恢复流使用。

在途写期限由会话定时器执行，支持并发缩短、延长和清除，避免 Gorilla 覆盖底层期限。写帧途中超时会关闭整个会话并返回 `os.ErrDeadlineExceeded`。

## Cookie 持久化

`cookiejar.New(nil)` 和零值 Jar 默认使用公共后缀表，阻止 `.co.uk` 等跨站 Cookie。`Restore(nil)` 在加载旧数据后恢复默认后缀表并重建索引；零值/反序列化后的 Jar 在首次读写时也会完成恢复。`SetPublicSuffixList` 会同时迁移索引。

JSON 序列化自动取得加锁快照。BSON/xorm 等反射编码器应使用 `Snapshot()`，不要在并发流量期间直接读写 `Entries`。反序列化必须发生在实例投入并发使用前。Cookie 的 Quoted 标记、IPv6 host-only 语义及超长 MaxAge 都会保留或按合法范围处理。
