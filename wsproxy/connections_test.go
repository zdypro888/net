package wsproxy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 同一设备预建的通道要聚合，活动通道不消失，其他租户及共享口令不可外泄。
func TestConnectionsTokenGroupsAndIsolates(t *testing.T) {
	s := NewServer()
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		s.sessions.PushBack(&slaverEntry{scope: "tenant-a-secret", session: &Session{Id: "client-a", sourceIP: "203.0.113.1", connectedAt: now}})
	}
	s.active[&Session{Id: "client-a", sourceIP: "203.0.113.1", connectedAt: now.Add(-time.Minute)}] = "tenant-a-secret"
	s.sessions.PushBack(&slaverEntry{scope: "tenant-b-secret", session: &Session{Id: "hidden-client", sourceIP: "203.0.113.2", connectedAt: now}})
	rows := s.ConnectionsToken("tenant-a-secret")
	if len(rows) != 1 || rows[0].Idle != 10 || rows[0].Active != 1 || !rows[0].ConnectedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("unexpected snapshot: %+v", rows)
	}
	data, _ := json.Marshal(rows)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "hidden-client") {
		t.Fatal("snapshot leaked scope or tenant")
	}
	if len(s.ConnectionsToken("unknown")) != 0 || s.sessions.Len() != 11 || len(s.active) != 1 {
		t.Fatal("snapshot changed connections or crossed scope")
	}
}
