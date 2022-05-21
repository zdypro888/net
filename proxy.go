package net

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/zdypro888/net/socks5"
	"github.com/zdypro888/utils"
)

type Conn = net.Conn

//HTTPDebugProxy 调试代理
var HTTPDebugProxy = &Proxy{Address: "http://127.0.0.1:8888"}

//Proxy 代理
type Proxy struct {
	Address string `bson:"Address" json:"Address"`
}

//LoadProxys 从文件读取所有代理信息
func LoadProxys(i interface{}) ([]*Proxy, error) {
	proxys := make([]*Proxy, 0)
	if err := utils.ReadLines(i, func(line string) error {
		proxys = append(proxys, &Proxy{Address: line})
		return nil
	}); err != nil {
		return nil, err
	}
	return proxys, nil
}

//GetProxyURL 取得代理地址
func (proxy *Proxy) GetProxyURL(req *http.Request) (*url.URL, error) {
	address, err := utils.RandomTemplateText(proxy.Address)
	if err != nil {
		return nil, err
	}
	return url.Parse(address)
}

//Dial 拨号
func (proxy *Proxy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	paddress, err := utils.RandomTemplateText(proxy.Address)
	if err != nil {
		return nil, err
	}
	proxyURL, err := url.Parse(paddress)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme == "socks5" {
		d := socks5.NewDialer("tcp", proxyURL.Host)
		if proxyURL.User != nil {
			auth := &socks5.UsernamePassword{
				Username: proxyURL.User.Username(),
			}
			auth.Password, _ = proxyURL.User.Password()
			d.AuthMethods = []socks5.AuthMethod{
				socks5.AuthMethodNotRequired,
				socks5.AuthMethodUsernamePassword,
			}
			d.Authenticate = auth.Authenticate
		}
		return d.DialContext(ctx, network, address)
	} else if proxyURL.Scheme == "http" || proxyURL.Scheme == "https" {
		conn, err := net.Dial("tcp", proxyURL.Host)
		if err != nil {
			return nil, err
		}
		connectReq, err := http.NewRequestWithContext(ctx, "CONNECT", address, nil)
		if err != nil {
			return nil, err
		}
		connectReq.URL = &url.URL{Opaque: address}
		connectReq.Host = address
		connectReq.Header = make(http.Header)
		if proxyURL.User != nil {
			auth := proxyURL.User.String()
			connectReq.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
		}
		if err = connectReq.Write(conn); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		var response *http.Response
		if response, err = http.ReadResponse(br, connectReq); err != nil {
			conn.Close()
			return nil, err
		}
		if response.StatusCode != 200 {
			conn.Close()
			return nil, fmt.Errorf("connect http tunnel faild: %d", response.StatusCode)
		}
		return conn, nil
	}
	return nil, fmt.Errorf("type: %s not supported", proxyURL.Scheme)
}
