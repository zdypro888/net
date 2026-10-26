package wsproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 同用户两台设备共享业务出口池，但可单独撤销；设备注册凭据不能升级成远程拨号凭据。
func TestDeviceRegistrationScopeAndIndependentRevocation(t *testing.T) {
	proxy := NewServer()
	proxy.ScopeByToken = true
	defer proxy.CloseAll()
	proxy.AuthorizeToken = func(_ context.Context, key string) error {
		if key == "owner-scope" {
			return nil
		}
		return errors.New("device cannot dial")
	}
	proxy.ResolveRegistrationToken = func(_ context.Context, key string) (RegistrationIdentity, error) {
		if key == "device-a" || key == "device-b" {
			return RegistrationIdentity{Scope: "owner-scope", ClientID: key}, nil
		}
		return RegistrationIdentity{}, errors.New("unauthorized")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e == nil {
			proxy.OnConnection(c)
		}
	}))
	defer server.Close()
	register := func(key string, method MethodType) *websocket.Conn {
		c, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { c.Close() })
		if e = c.WriteJSON(connPacket{Id: "untrusted-self-reported", Method: method, Token: key}); e != nil {
			t.Fatal(e)
		}
		return c
	}
	_ = register("device-a", MethodRegisterSlaver)
	_ = register("device-b", MethodRegisterSlaver)
	wait := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && proxy.ConnectionCountToken("owner-scope") != want {
			time.Sleep(time.Millisecond)
		}
		if n := proxy.ConnectionCountToken("owner-scope"); n != want {
			t.Fatalf("connections=%d want=%d", n, want)
		}
	}
	_ = register("device-a", MethodRegisterSlaver)
	wait(3)
	rows := proxy.ConnectionsToken("owner-scope")
	if len(rows) != 2 || rows[0].ClientID != "device-a" || rows[0].Idle != 2 || rows[1].ClientID != "device-b" {
		t.Fatal("trusted device IDs were not applied", rows)
	}
	proxy.CloseCredentialHash(credentialHash("device-a"))
	wait(1)
	denied := register("device-b", MethodClientDialout)
	_ = denied.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, e := denied.ReadMessage(); e == nil {
		t.Fatal("device token gained remote dial")
	}
	// 用户池变更等同撤权，复核需要同时关闭活动隧道。
	entry, e := proxy.popSession("owner-scope")
	if e != nil {
		t.Fatal(e)
	}
	proxy.ResolveRegistrationToken = func(context.Context, string) (RegistrationIdentity, error) {
		return RegistrationIdentity{Scope: "other-owner"}, nil
	}
	proxy.RevalidateRegistrations(context.Background())
	entry.session.deadlineMu.Lock()
	closed := entry.session.closed
	entry.session.deadlineMu.Unlock()
	if !closed {
		t.Fatal("active revoked tunnel remained open")
	}
}
