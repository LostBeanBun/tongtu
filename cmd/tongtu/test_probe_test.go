package main

// tongtu test 的「检测口径」回归：经代理 / 无SNI反代两列以运行实例回报的
// 实际状态为准，配置只是拿不到实例时的兜底（见 probeTargets 的注释）。

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"tongtu/internal/app"
	"tongtu/internal/config"
)

// TestProbeTargetsPrefersRunningInstance 端口回退 / 运行期配置编辑之后，
// 检测必须跟随实例的实际情况：拿 cfg.Listen 当现实会把健康代理报成没在跑。
func TestProbeTargetsPrefersRunningInstance(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:19180" // 配置意图
	cfg.MITM.Enabled = false       // 配置里没开反代
	cfg.Custom.MITMDomains = []string{"my.site.com"}

	// 实例实际监听在回退端口、且运行时开了反代
	st := &app.Status{Listen: "127.0.0.1:29180", MITM: true}
	listen, mitmOn, patterns := probeTargets(cfg, st)
	if listen != "127.0.0.1:29180" {
		t.Errorf("应使用实例回报的实际监听地址，实际 %q（cfg.Listen 只是意图）", listen)
	}
	if !mitmOn {
		t.Error("反代开关应以实例运行态为准（cfg.MITM.Enabled 只是配置意图）")
	}
	// 域名表始终是生效全集：漏掉自定义站点会把它们误报成「未加速」
	if !slices.Contains(patterns, "my.site.com") {
		t.Errorf("域名表应含自定义 mitm_domains，实际 %v", patterns)
	}
	if !slices.Contains(patterns, cfg.MITM.Domains[0]) {
		t.Errorf("域名表应含内置 mitm.domains，实际 %v", patterns)
	}
}

// TestProbeTargetsFallbackToConfig 拿不到实例（未运行 / 响应里没有监听地址）
// 时退回配置——此时本来也没有经代理可测，口径错误只影响「配置就是对的」那一半。
func TestProbeTargetsFallbackToConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:19180"
	cfg.MITM.Enabled = true

	for name, st := range map[string]*app.Status{
		"无实例":    nil,
		"空状态响应":  {},
		"监听地址为空": {MITM: false},
	} {
		listen, mitmOn, _ := probeTargets(cfg, st)
		if listen != cfg.Listen {
			t.Errorf("%s: 应退回 cfg.Listen，实际 %q", name, listen)
		}
		if mitmOn != cfg.MITM.Enabled {
			t.Errorf("%s: 应退回 cfg.MITM.Enabled，实际 %v", name, mitmOn)
		}
	}
}

// TestRunningStatusNilWhenNotTongtu 拿状态失败只允许「退回配置」，
// 不允许把 test 命令整个弄失败：端口没人听、被别的 HTTP 程序占用都返回 nil。
func TestRunningStatusNilWhenNotTongtu(t *testing.T) {
	cfg := config.Default()

	// 端口没人监听
	cfg.Control = "127.0.0.1:1"
	if st := runningStatus(cfg); st != nil {
		t.Errorf("没人监听时应返回 nil，实际 %+v", st)
	}

	// 被一个非通途的 HTTP 服务占用
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("我是别的程序"))
	}))
	defer srv.Close()
	cfg.Control = strings.TrimPrefix(srv.URL, "http://")
	if st := runningStatus(cfg); st != nil {
		t.Errorf("非通途 HTTP 服务占用时应返回 nil，实际 %+v", st)
	}
}
