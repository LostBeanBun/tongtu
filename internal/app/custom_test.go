package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"tongtu/internal/config"
	"tongtu/internal/rules"
)

// newCustomTestApp 起一个不接管系统代理 / hosts / 反代的实例，配置写到临时目录。
//
// 必须指定 ConfigPath：增删自定义站点会写回配置文件，测试绝不能碰真实的
// %APPDATA%\tongtu\config.yaml。
func newCustomTestApp(t *testing.T) (*App, string) {
	t.Helper()
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.Hosts, cfg.MITM.Enabled = false, false, false
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("保存初始配置: %v", err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, Log: quiet, ConfigPath: path})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a, path
}

// TestCustomSiteLifecycle 添加 / 打开反代 / 删除：每一步都要写回配置并立即生效。
func TestCustomSiteLifecycle(t *testing.T) {
	a, path := newCustomTestApp(t)

	if got := a.CustomSites(); len(got) != 0 {
		t.Fatalf("初始不该有自定义站点: %v", got)
	}

	// 粘贴完整网址也要能加（界面上的常见操作），存进去的是主机名。
	p, err := a.AddCustomSite("  https://My.Site.com/some/path?q=1  ")
	if err != nil {
		t.Fatalf("AddCustomSite: %v", err)
	}
	if p != "my.site.com" {
		t.Fatalf("应规范化为 my.site.com，实际 %q", p)
	}
	sites := a.CustomSites()
	if len(sites) != 1 || sites[0].Pattern != "my.site.com" || sites[0].MITM {
		t.Fatalf("添加后列表不对: %+v", sites)
	}
	// 立即生效：规则集里已经能匹配到它（不用重启）。
	if !a.rs.Match("my.site.com") {
		t.Error("新加的自定义站点应立即进入白名单")
	}

	// 重复添加给明确错误，而不是静默成功。
	if _, err := a.AddCustomSite("my.site.com"); err == nil {
		t.Error("重复添加应报错")
	}
	// 写法错误同样报错（校验复用配置的规则）。
	if _, err := a.AddCustomSite("my.*.site.com"); err == nil {
		t.Error("错误的通配写法应报错")
	}

	// 打开反代：同时进反代名单与白名单。
	if err := a.SetCustomSiteMITM("my.site.com", true); err != nil {
		t.Fatalf("SetCustomSiteMITM: %v", err)
	}
	sites = a.CustomSites()
	if len(sites) != 1 || !sites[0].MITM {
		t.Fatalf("打开反代后列表不对: %+v", sites)
	}

	// 配置已写回：重新加载能看到同样的内容。
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("重新加载配置: %v", err)
	}
	if !containsString(cfg.Custom.Domains, "my.site.com") ||
		!containsString(cfg.Custom.MITMDomains, "my.site.com") {
		t.Fatalf("配置未写回: %+v", cfg.Custom)
	}
	if !rules.MatchPatterns("my.site.com", cfg.MITMPatterns()) {
		t.Error("配置里的反代名单应包含该域名")
	}

	// 关掉反代只影响反代名单，白名单保留。
	if err := a.SetCustomSiteMITM("my.site.com", false); err != nil {
		t.Fatalf("关闭反代: %v", err)
	}
	cfg, _ = config.Load(path)
	if !containsString(cfg.Custom.Domains, "my.site.com") ||
		containsString(cfg.Custom.MITMDomains, "my.site.com") {
		t.Fatalf("关闭反代后的配置不对: %+v", cfg.Custom)
	}

	// 删除：两个名单都要清掉。
	if err := a.RemoveCustomSite("my.site.com"); err != nil {
		t.Fatalf("RemoveCustomSite: %v", err)
	}
	if got := a.CustomSites(); len(got) != 0 {
		t.Fatalf("删除后列表应为空: %+v", got)
	}
	if a.rs.Match("my.site.com") {
		t.Error("删除后不该再匹配该域名")
	}
	cfg, _ = config.Load(path)
	if containsString(cfg.Custom.Domains, "my.site.com") ||
		containsString(cfg.Custom.MITMDomains, "my.site.com") {
		t.Fatalf("删除未写回配置: %+v", cfg.Custom)
	}

	// 删除不存在的项要报错（界面据此提示，而不是假装成功）。
	if err := a.RemoveCustomSite("my.site.com"); err == nil {
		t.Error("删除不存在的站点应报错")
	}
}

// TestCustomSitesMergesMITMOnly 只出现在反代名单里的域名也要显示出来（否则界面里删不掉）。
func TestCustomSitesMergesMITMOnly(t *testing.T) {
	got := customSitesOf(config.CustomConfig{
		Domains:     []string{"a.com", "b.com"},
		MITMDomains: []string{"b.com", "c.com"},
	})
	want := []CustomSite{
		{Pattern: "a.com", MITM: false},
		{Pattern: "b.com", MITM: true},
		{Pattern: "c.com", MITM: true},
	}
	if len(got) != len(want) {
		t.Fatalf("合并后应有 %d 项，实际 %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项应为 %+v，实际 %+v", i, want[i], got[i])
		}
	}
}
