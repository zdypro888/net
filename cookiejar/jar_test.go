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
	jar.SetPublicSuffixList(testPublicSuffixList{})

	foo := mustParseURL(t, "https://foo.co.uk/")
	bar := mustParseURL(t, "https://bar.co.uk/")
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
	jar.SetCookies(mustParseURL(t, "https://example.com/"), []*http.Cookie{{Name: "a", Value: "1"}})

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
}
