package cookiejar

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type testPublicSuffixList struct{}

func (testPublicSuffixList) PublicSuffix(domain string) string {
	if domain == "co.uk" || strings.HasSuffix(domain, ".co.uk") {
		return "co.uk"
	}
	if i := strings.LastIndex(domain, "."); i >= 0 {
		return domain[i+1:]
	}
	return domain
}

func (testPublicSuffixList) String() string {
	return "test"
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestZeroValueJarSetCookiesNilEntries 回归: 模拟从存储反序列化得到的空 Jar
// (Entries==nil, psList==nil)。setCookies 写入前必须惰性初始化 Entries, 否则
// nil map 赋值 panic; 同时 SetPublicSuffixList 设回 psList 后公共后缀保护生效。
func TestZeroValueJarSetCookiesNilEntries(t *testing.T) {
	var jar Jar // Entries==nil, 等价于反序列化得到的空 jar
	// 零值与 New(nil) 都应默认启用后缀隔离，无需调用方另行安装保护。
	foo := mustParseURL(t, "https://foo.co.uk/")
	bar := mustParseURL(t, "https://bar.co.uk/")
	for _, j := range []*Jar{&jar, func() *Jar { j, _ := New(nil); return j }()} {
		j.SetCookies(foo, []*http.Cookie{{Name: "cross-site", Domain: "co.uk", Value: "bad"}})
		if got := j.Cookies(bar); len(got) != 0 {
			t.Errorf("default suffix isolation failed: %v", got)
		}
	}
	jar.SetPublicSuffixList(testPublicSuffixList{})

	jar.SetCookies(foo, []*http.Cookie{{
		Name:   "public-suffix",
		Value:  "1",
		Domain: "co.uk",
	}})
	if got := jar.Cookies(bar); len(got) != 0 {
		t.Fatalf("public suffix cookie accepted: got %v", got)
	}

	// host-only cookie: 在 Entries==nil 的 Jar 上 SetCookies 不得 panic, 且能存取。
	jar.SetCookies(foo, []*http.Cookie{{
		Name:  "host-only",
		Value: "1",
	}})
	if got := jar.Cookies(foo); len(got) != 1 || got[0].Name != "host-only" {
		t.Fatalf("nil-Entries jar did not store host-only cookie: got %v", got)
	}
	// 忽略 nil cookie，超长 MaxAge 不得经 time.Duration 溢出为过期时间。
	jar.SetCookies(foo, []*http.Cookie{nil, {Name: "long-lived", Value: "1", MaxAge: int(^uint(0) >> 1)}})
	found := false
	for _, c := range jar.Cookies(foo) {
		if c.Name == "long-lived" {
			found = true
		}
	}
	if !found {
		t.Fatal("large MaxAge cookie disappeared")
	}
	ipv6 := mustParseURL(t, "https://[::1]/")
	jar.SetCookies(ipv6, []*http.Cookie{{Name: "ipv6", Domain: "::1", Value: "ok"}})
	if got := jar.Cookies(mustParseURL(t, "https://[::1]:443/")); len(got) != 1 || got[0].Name != "ipv6" {
		t.Errorf("IPv6 host/port identity mismatch: %v", got)
	}
	zone := mustParseURL(t, "https://[::1%25zone.example.com]:443/")
	jar.SetCookies(zone, []*http.Cookie{{Name: "zone-escape", Domain: "example.com", Value: "bad"}})
	if got := jar.Cookies(mustParseURL(t, "https://www.example.com/")); len(got) != 0 {
		t.Errorf("IPv6 zone escaped into DNS cookies: %v", got)
	}

}

func TestJarSnapshotDeepCopiesPersistentFields(t *testing.T) {
	jar, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u := mustParseURL(t, "https://example.com/")
	jar.SetCookies(u, []*http.Cookie{{Name: "a", Value: "1"}})

	snapshot := jar.Snapshot()
	jar.SetCookies(u, []*http.Cookie{{Name: "b", Value: "2"}})
	if snapshot.NextSeqNum != 1 {
		t.Fatalf("snapshot NextSeqNum = %d, want 1", snapshot.NextSeqNum)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("snapshot entries len = %d, want 1", len(snapshot.Entries))
	}
	for _, submap := range snapshot.Entries {
		if len(submap) != 1 {
			t.Fatalf("snapshot submap len = %d, want 1", len(submap))
		}
		for id, entry := range submap {
			entry.Value = "mutated"
			submap[id] = entry
		}
	}
	if got := jar.Cookies(u); len(got) != 2 {
		t.Fatalf("snapshot mutation affected jar or second cookie missing: got %v", got)
	}
}

func TestJarMarshalJSONUsesLockedSnapshot(t *testing.T) {
	jar, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(mustParseURL(t, "https://example.com/"), []*http.Cookie{{Name: "a", Value: "1", Quoted: true}})

	data, err := json.Marshal(jar)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot JarSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.NextSeqNum != 1 || len(snapshot.Entries) != 1 {
		t.Fatalf("unexpected JSON snapshot: seq=%d entries=%d raw=%s", snapshot.NextSeqNum, len(snapshot.Entries), data)
	}
	var restored Jar
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	got := restored.Cookies(mustParseURL(t, "https://example.com/"))
	if len(got) != 1 || !got[0].Quoted {
		t.Fatalf("quoted cookie did not survive persistence: %v", got)
	}
	// 旧版 nil-PSL 桶用 co.uk，新版默认桶是 foo.co.uk；恢复必须迁移，不能丢登录态。
	legacy := Jar{Entries: map[string]map[string]Entry{"co.uk": {"foo.co.uk;/;legacy": {Name: "legacy", Value: "v", Domain: "foo.co.uk", Path: "/", HostOnly: true}}}}
	legacy.Restore(nil)
	if got := legacy.Cookies(mustParseURL(t, "https://foo.co.uk/")); len(got) != 1 || got[0].Name != "legacy" {
		t.Fatalf("legacy cookie indexing was lost: %v", got)
	}

}
