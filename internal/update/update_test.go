package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.0", "0.4.0", -1},
		{"0.4.0", "0.4.0", 0},
		{"v0.4.0", "0.4.0", 0},
		{"0.4.1", "0.4.0", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.4", "0.4.0", 0},
		{"0.4.0-dev", "0.4.0", -1},
		{"0.4.0", "0.4.0-rc1", 1},
		{"0.4.0-rc1", "0.4.0-rc2", -1},
		{"0.4.0+build7", "0.4.0", 0},
	}
	for _, c := range cases {
		got, err := Compare(c.a, c.b)
		if err != nil {
			t.Errorf("Compare(%q, %q) 报错: %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareInvalid(t *testing.T) {
	for _, s := range []string{"", "abc", "1.2.3.4", "1..2", "-1.0.0"} {
		if _, err := Compare(s, "0.4.0"); err == nil {
			t.Errorf("Compare(%q, ...) 应报错", s)
		}
	}
}

// releaseServer 返回一个假的 GitHub API：latest 路径按给定状态码与响应体返回。
func releaseServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/name/releases/latest" {
			t.Errorf("请求路径不对: %s", r.URL.Path)
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "tongtu/") {
			t.Errorf("User-Agent 应带版本号: %q", ua)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestCheckNewer(t *testing.T) {
	body := `{"tag_name":"v0.4.1","html_url":"https://github.com/owner/name/releases/tag/v0.4.1","published_at":"2026-10-20T10:00:00Z"}`
	srv := releaseServer(t, http.StatusOK, body)
	defer srv.Close()

	c := Checker{BaseURL: srv.URL, Repo: "owner/name", Client: srv.Client()}
	res, err := c.Check(context.Background(), "0.4.0")
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	if res.Latest != "0.4.1" {
		t.Errorf("Latest = %q，期望 0.4.1", res.Latest)
	}
	if !res.Newer {
		t.Error("0.4.1 比 0.4.0 新，Newer 应为 true")
	}
	if res.URL != "https://github.com/owner/name/releases/tag/v0.4.1" {
		t.Errorf("URL = %q", res.URL)
	}
	if res.Published.IsZero() {
		t.Error("应解析出发布时间")
	}
	if res.Current != "0.4.0" {
		t.Errorf("Current = %q", res.Current)
	}
}

func TestCheckSameVersion(t *testing.T) {
	body := `{"tag_name":"0.4.0","html_url":"https://example.test/r"}`
	srv := releaseServer(t, http.StatusOK, body)
	defer srv.Close()

	res, err := Checker{BaseURL: srv.URL, Repo: "owner/name", Client: srv.Client()}.Check(context.Background(), "v0.4.0")
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	if res.Newer {
		t.Error("同版本不应报告有新版")
	}
	if res.Latest != "0.4.0" {
		t.Errorf("Latest = %q", res.Latest)
	}
}

func TestCheckNoRelease(t *testing.T) {
	srv := releaseServer(t, http.StatusNotFound, `{"message":"Not Found"}`)
	defer srv.Close()

	_, err := Checker{BaseURL: srv.URL, Repo: "owner/name", Client: srv.Client()}.Check(context.Background(), "0.4.0")
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("404 应返回 ErrNoRelease，实际 %v", err)
	}
}

func TestCheckRateLimited(t *testing.T) {
	srv := releaseServer(t, http.StatusForbidden, `{"message":"API rate limit exceeded"}`)
	defer srv.Close()

	_, err := Checker{BaseURL: srv.URL, Repo: "owner/name", Client: srv.Client()}.Check(context.Background(), "0.4.0")
	if err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("403 应提示限流，实际 %v", err)
	}
}

func TestCheckServerError(t *testing.T) {
	srv := releaseServer(t, http.StatusInternalServerError, `{}`)
	defer srv.Close()

	_, err := Checker{BaseURL: srv.URL, Repo: "owner/name", Client: srv.Client()}.Check(context.Background(), "0.4.0")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("500 应报告状态码，实际 %v", err)
	}
}

func TestCheckBadPayload(t *testing.T) {
	cases := []string{
		`not json`,
		`{"tag_name":""}`,
		`{"tag_name":"vNext"}`,
	}
	for _, body := range cases {
		srv := releaseServer(t, http.StatusOK, body)
		_, err := Checker{BaseURL: srv.URL, Repo: "owner/name", Client: srv.Client()}.Check(context.Background(), "0.4.0")
		srv.Close()
		if err == nil {
			t.Errorf("响应 %q 应报错", body)
		}
	}
}

// TestCheckNetworkError 覆盖断网：错误里要带「无法连接 GitHub」，别丢一个裸的 URL 错误。
func TestCheckNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // 先关掉，制造连接失败

	_, err := Checker{BaseURL: url, Repo: "owner/name", Client: &http.Client{}}.Check(context.Background(), "0.4.0")
	if err == nil || !strings.Contains(err.Error(), "无法连接 GitHub") {
		t.Fatalf("连接失败应给出可读提示，实际 %v", err)
	}
}
