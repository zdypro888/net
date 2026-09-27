package net

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zdypro888/net/cookiejar"
)

func TestConfigureCookieTypedNilDoesNotPanicOnRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "test"})
	}))
	defer server.Close()
	var empty *cookiejar.Jar
	for _, jar := range []http.CookieJar{nil, empty} {
		client := NewHTTP(nil)
		client.ConfigureCookie(jar)
		snapshot := client.ClientSnapshot()
		if snapshot.Jar != nil {
			t.Fatal("nil cookie jar retained inside interface")
		}
		resp, err := snapshot.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
}
