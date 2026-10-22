// Package update 提供「手动检查更新」：查询 GitHub 最新 Release 并与当前版本比较。
//
// 只做检查，不做下载与自动替换——通途是绿色单文件，更新就是「下新的 exe 覆盖旧的」；
// 自动替换正在运行的 exe 既不可靠也不安全，还需要额外处理文件占用与回滚。
// 用户可在托盘点「检查更新」，看到新版本后由浏览器打开 Release 页面自行下载。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNoRelease 表示仓库还没有发布任何 Release（404）。
// 这不是故障：项目早期只有 tag、没有 Release，检查更新应当平实地说明这一点。
var ErrNoRelease = errors.New("仓库还没有发布版本")

// Repo 是官方仓库（owner/name）。
const Repo = "LostBeanBun/tongtu"

// maxBody 限制响应体大小：GitHub 的 Release JSON 只有几 KB，读太多纯属浪费。
const maxBody = 256 << 10

// Result 是一次检查的结论。
type Result struct {
	Current   string    // 当前版本（原样，可能带 -dev 后缀）
	Latest    string    // 最新版本（去掉 v 前缀）
	URL       string    // Release 页面（可直接用浏览器打开）
	Published time.Time // 发布时间（可能为零值）
	Newer     bool      // Latest 是否比 Current 新
}

// Checker 查询某个 GitHub 仓库的最新 Release。
// 做成结构体而不是全局客户端：测试可以指向 httptest 服务器，不必联网。
type Checker struct {
	BaseURL string       // 例如 https://api.github.com；测试指向本地服务器
	Repo    string       // owner/name
	Client  *http.Client // 为 nil 时用带 10 秒超时的默认客户端
}

// Default 返回官方仓库的检查器。
func Default() Checker {
	return Checker{
		BaseURL: "https://api.github.com",
		Repo:    Repo,
		Client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Check 查询最新 Release 并与 current 比较。
//
// 错误分三种，调用方要分开对待：
//   - ErrNoRelease：仓库还没发布版本（正常状态，不是网络故障）；
//   - 其他错误：网络不通 / 限流 / 响应异常，提示用户稍后再试；
//   - nil：结论在 Result 里（Newer 决定是「已是最新」还是「有新版」）。
func (c Checker) Check(ctx context.Context, current string) (Result, error) {
	res := Result{Current: current}
	base := strings.TrimRight(c.BaseURL, "/")
	repo := c.Repo
	if repo == "" {
		repo = Repo
	}
	url := base + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return res, err
	}
	// GitHub 要求带 User-Agent；版本号一并放进去，对方看日志时能区分是哪个版本在查。
	req.Header.Set("User-Agent", "tongtu/"+strings.TrimPrefix(current, "v"))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return res, fmt.Errorf("无法连接 GitHub：%w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return res, ErrNoRelease
	case http.StatusForbidden, http.StatusTooManyRequests:
		return res, errors.New("GitHub 拒绝了请求（可能触发了限流），请稍后再试")
	default:
		return res, fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}

	var payload struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return res, fmt.Errorf("读取响应失败：%w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return res, fmt.Errorf("响应不是合法 JSON：%w", err)
	}
	latest := strings.TrimSpace(payload.TagName)
	if latest == "" {
		return res, errors.New("响应里没有版本号（tag_name）")
	}
	if _, _, err := parseVersion(latest); err != nil {
		return res, fmt.Errorf("最新版本号无法解析：%w", err)
	}
	res.Latest = strings.TrimPrefix(strings.TrimPrefix(latest, "v"), "V")
	res.URL = payload.HTMLURL
	if res.URL == "" {
		res.URL = "https://github.com/" + repo + "/releases"
	}
	res.Published = payload.PublishedAt

	cmp, err := Compare(current, latest)
	if err != nil {
		return res, err
	}
	res.Newer = cmp < 0
	return res, nil
}

// Compare 比较两个语义化版本：a<b 返回 -1，相等返回 0，a>b 返回 1。
//
// 规则（够用即可，不追求完整的 semver 实现）：
//   - 忽略 v/V 前缀与 +build 元数据；
//   - 只比较主.次.修订三段数字，缺段按 0（"0.4" 等于 "0.4.0"）；
//   - 预发布后缀（-dev、-rc1）比同号正式版小：本地开发版看到同号正式发布时应当提示有新版。
func Compare(a, b string) (int, error) {
	x, preA, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	y, preB, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		switch {
		case x[i] < y[i]:
			return -1, nil
		case x[i] > y[i]:
			return 1, nil
		}
	}
	switch {
	case preA == preB:
		return 0, nil
	case preA == "":
		return 1, nil
	case preB == "":
		return -1, nil
	case preA < preB:
		return -1, nil
	default:
		return 1, nil
	}
}

// parseVersion 拆出三段数字与预发布后缀。
func parseVersion(s string) ([3]int, string, error) {
	var nums [3]int
	orig := s
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return nums, "", fmt.Errorf("版本号格式不对: %q（应形如 0.4.1）", orig)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nums, "", fmt.Errorf("版本号格式不对: %q（应形如 0.4.1）", orig)
		}
		nums[i] = n
	}
	return nums, pre, nil
}
