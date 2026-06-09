package cookiejar

import (
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
