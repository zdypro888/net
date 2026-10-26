package wsproxy

import (
	"fmt"
	"testing"
)

func nodeTestChannel(s *Server, scope, node, id string) *Session {
	session := &Session{Id: node, sourceIP: "203.0.113.1"}
	// id 只用于测试区分同节点内部顺序，不参与节点调度。
	session.authorization = id
	s.sessions.PushBack(&slaverEntry{scope: scope, session: session, reply: make(chan slaverRead, 1)})
	return session
}
func TestNodeRotationIgnoresChannelCountsAndKeepsFIFO(t *testing.T) {
	server := NewServer()
	defer server.cancel()
	for i := 0; i < 10; i++ {
		nodeTestChannel(server, "tenant-a", "A", fmt.Sprintf("A%d", i))
	}
	for i := 0; i < 3; i++ {
		nodeTestChannel(server, "tenant-a", "B", fmt.Sprintf("B%d", i))
	}
	for i := 0; i < 3; i++ {
		nodeTestChannel(server, "tenant-a", "C", fmt.Sprintf("C%d", i))
	}
	other := nodeTestChannel(server, "tenant-b", "A", "other")
	for i, want := range []string{"A0", "B0", "C0", "A1", "B1", "C1", "A2", "B2", "C2"} {
		entry, err := server.popSession("tenant-a")
		if err != nil || entry.session.authorization != want {
			t.Fatalf("step %d: %v %v", i, entry, err)
		}
	}
	if entry, err := server.popSession("tenant-b"); err != nil || entry.session != other {
		t.Fatal("cross-tenant rotation", err)
	}
}
func TestNodeRotationReplenishmentAndNoIdleNode(t *testing.T) {
	server := NewServer()
	defer server.cancel()
	nodeTestChannel(server, "tenant", "A", "first")
	for i := 0; i < 5; i++ {
		nodeTestChannel(server, "tenant", "B", fmt.Sprintf("B%d", i))
	}
	for i, want := range []string{"A", "B", "A", "B", "A", "B"} {
		entry, err := server.popSession("tenant")
		if err != nil || entry.session.Id != want {
			t.Fatal(i, err, entry)
		}
		if want == "A" {
			nodeTestChannel(server, "tenant", "A", "refill")
		}
	}
	// 节点没有空闲通道时，继续使用可用节点；不会因轮换位置等待空节点。
	for server.sessions.Len() > 0 {
		if _, err := server.popSession("tenant"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := server.popSession("tenant"); err != ErrNoConnection {
		t.Fatal(err)
	}
}
