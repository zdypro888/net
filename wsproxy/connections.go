package wsproxy

import (
	"sort"
	"time"
)

// ConnectionInfo 按客户端标识与接入 IP 汇总通道，不包含 token、目的地址或连接对象。
// 同一客户端通常预建多条待命通道，不能把通道数解释为设备数或出口 IP 数。
type ConnectionInfo struct {
	ClientID    string    `json:"clientId"`
	SourceIP    string    `json:"sourceIP"`
	ConnectedAt time.Time `json:"connectedAt"`
	Idle        int       `json:"idle"`
	Active      int       `json:"active"`
}

// ConnectionsToken 只返回指定租户池的运行时快照；读取不领取、断开或探测任何通道。
func (server *Server) ConnectionsToken(token string) []ConnectionInfo {
	server.locker.Lock()
	defer server.locker.Unlock()
	groups := make(map[[2]string]*ConnectionInfo)
	add := func(session *Session, active bool) {
		key := [2]string{session.Id, session.sourceIP}
		row := groups[key]
		if row == nil {
			row = &ConnectionInfo{ClientID: session.Id, SourceIP: session.sourceIP, ConnectedAt: session.connectedAt}
			groups[key] = row
		}
		if session.connectedAt.Before(row.ConnectedAt) {
			row.ConnectedAt = session.connectedAt
		}
		if active {
			row.Active++
		} else {
			row.Idle++
		}
	}
	for elem := server.sessions.Front(); elem != nil; elem = elem.Next() {
		entry := elem.Value.(*slaverEntry)
		if entry.scope == token {
			add(entry.session, false)
		}
	}
	for session, scope := range server.active {
		if scope == token {
			add(session, true)
		}
	}
	rows := make([]ConnectionInfo, 0, len(groups))
	for _, row := range groups {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SourceIP != rows[j].SourceIP {
			return rows[i].SourceIP < rows[j].SourceIP
		}
		return rows[i].ClientID < rows[j].ClientID
	})
	return rows
}
