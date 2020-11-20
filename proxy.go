package net

import (
	"strings"

	"github.com/zdypro888/utils"
)

//ProxyType 代理种类
type ProxyType int

const (
	//HTTP HTTP代理
	HTTP ProxyType = 0
	//SOCKS5 Socks5代理
	SOCKS5 ProxyType = 1
)

//Proxy 代理
type Proxy struct {
	Type     ProxyType `bson:"Type" json:"Type"`
	Address  string    `bson:"Address" json:"Address"`
	UserName string    `bson:"UserName" json:"UserName"`
	Password string    `bson:"Password" json:"Password"`
}

//LoadProxys 从文件读取所有代理信息
func LoadProxys(pt ProxyType, i interface{}) ([]*Proxy, error) {
	proxys := make([]*Proxy, 0)
	if err := utils.ReadLines(i, func(line string) error {
		if ua := strings.Split(line, "@"); len(ua) == 2 {
			if up := strings.Split(ua[0], ":"); len(up) == 2 {
				proxys = append(proxys, &Proxy{Type: pt, Address: ua[1], UserName: up[0], Password: up[1]})
			}
		} else if len(ua) == 1 {
			proxys = append(proxys, &Proxy{Type: pt, Address: ua[0]})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return proxys, nil
}
