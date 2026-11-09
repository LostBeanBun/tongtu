package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestPlainHTTPStripsAltSvc 普通 HTTP 转发也应去掉上游的 Alt-Svc 广告：
// 本地代理只处理 TCP，留着它会让浏览器去试注定失败（或绕过代理）的 QUIC。
func TestPlainHTTPStripsAltSvc(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":443"; ma=86400`)
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	p := NewWithLookup(mustRules(t), failLookup(t, "HTTP 转发"), discardLog())
	px := httptest.NewServer(p)
	defer px.Close()

	pxURL, _ := url.Parse(px.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pxURL)}}
	resp, err := client.Get(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Fatalf("Alt-Svc 应被剥离，实际为 %q", got)
	}
}
