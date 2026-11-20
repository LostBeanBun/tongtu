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

// TestAppLifecycle 验证不接管系统代理时的生命周期：
// 启动可监听、状态正确、Shutdown 幂等且能真正停止。
func TestAppLifecycle(t *testing.T) {
	rs, err := rules.Load([]string{"steam", "github"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0" // 随机端口，避免与真实实例冲突

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, Log: quiet})

	if a.Status().Running {
		t.Fatal("创建后不应为运行中")
	}
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := a.Status()
	if !st.Running {
		t.Fatal("启动后应为运行中")
	}
	if st.Listen == "" || st.Listen == cfg.Listen {
		t.Fatalf("监听地址应已更新为实际端口: %q", st.Listen)
	}
	if st.SystemProxy {
		t.Fatalf("未请求接管时不应开启: sys=%v", st.SystemProxy)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if a.Status().Running {
		t.Fatal("停止后不应为运行中")
	}
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("重复 Shutdown 应无错: %v", err)
	}
}

// TestStartTwiceFails 重复启动应报错。
func TestStartTwiceFails(t *testing.T) {
	rs, _ := rules.Load([]string{"steam"})
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, Log: quiet})

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()
	if err := a.Start(); err == nil {
		t.Fatal("重复启动应报错")
	}
}

// TestRuntimeTogglesPersist 覆盖托盘菜单用到的运行时开关：
// 切换服务组会更新状态并持久化；不接管系统代理时各项开关也能安全写入配置。
func TestRuntimeTogglesPersist(t *testing.T) {
	rs, err := rules.Load([]string{"steam", "github"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.MITM.Enabled = false, false
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, ConfigPath: cfgPath, Log: quiet})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	if got := a.ConfigPath(); got != cfgPath {
		t.Fatalf("ConfigPath 应为 %q，实际 %q", cfgPath, got)
	}

	if err := a.SetServices([]string{"steam"}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}
	if st := a.Status(); len(st.Services) != 1 || st.Services[0] != "steam" {
		t.Fatalf("服务组未更新: %v", st.Services)
	}
	if err := a.SetServices(nil); err == nil {
		t.Fatal("空服务列表应被拒绝")
	}

	if err := a.SetMITM(false); err != nil { // 未启用时是安全的空操作
		t.Fatalf("SetMITM(false): %v", err)
	}
	if err := a.SetMode(false); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新加载配置: %v", err)
	}
	if len(saved.Services) != 1 || saved.Services[0] != "steam" {
		t.Fatalf("服务组未持久化: %v", saved.Services)
	}
	if saved.SystemProxy || saved.MITM.Enabled {
		t.Fatalf("开关未持久化: sys=%v mitm=%v", saved.SystemProxy, saved.MITM.Enabled)
	}
}

// TestOneShotFlagsNotPersisted 一次性启动标志（--no-sysproxy）不应被
// 其它托盘开关触发的持久化误写进配置。
func TestOneShotFlagsNotPersisted(t *testing.T) {
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	// 配置里开着；启动时用 --no-sysproxy 临时关掉，不能写回配置。
	cfg.SystemProxy = true
	cfg.Listen = "127.0.0.1:0"
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 模拟 tongtu start --no-sysproxy
	a := New(Options{
		Config: cfg, Rules: rs, ConfigPath: cfgPath, Log: quiet,
		SysProxy: false,
	})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	if err := a.SetServices([]string{"steam"}); err != nil { // 会触发配置持久化
		t.Fatalf("SetServices: %v", err)
	}
	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新加载配置: %v", err)
	}
	if !saved.SystemProxy {
		t.Fatalf("一次性标志被误写入配置: sys=%v", saved.SystemProxy)
	}
}

// TestSaveConfigPreservesDiskEdits 【中1】运行期间用户改过的非热字段不能被回滚：
// 托盘切换只覆盖运行时权威的五个字段（SystemProxy / MITM / Services / Custom），
// 端口、DNS 端点这类「只有改配置文件才更新」的字段一律以磁盘为准。
func TestSaveConfigPreservesDiskEdits(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	// 启动那一刻磁盘上的配置（模拟启动时 loadConfig 读到的那份）
	start := config.Default()
	start.Listen = "127.0.0.1:0"
	start.SystemProxy = true
	if err := config.Save(cfgPath, start); err != nil {
		t.Fatalf("写初始配置: %v", err)
	}

	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy = true
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, ConfigPath: cfgPath, Log: quiet})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	// 运行期间用户直接改了配置文件里的非热字段（watcher 对这些字段不热更新）
	edited, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	edited.Listen = "127.0.0.1:29999"
	edited.DNS.Endpoints = []string{"https://user-edited.example/resolve"}
	if err := config.Save(cfgPath, edited); err != nil {
		t.Fatalf("用户改配置: %v", err)
	}

	// 托盘切换服务组 → 触发持久化
	if err := a.SetServices([]string{"github"}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}

	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新加载配置: %v", err)
	}
	// 非热字段：以磁盘为准，不能被启动时的内存快照回滚
	if saved.Listen != "127.0.0.1:29999" {
		t.Errorf("运行期间改的监听地址被回滚成 %q（用户已改成 127.0.0.1:29999）", saved.Listen)
	}
	if len(saved.DNS.Endpoints) != 1 || saved.DNS.Endpoints[0] != "https://user-edited.example/resolve" {
		t.Errorf("运行期间改的 DNS 端点被回滚: %v", saved.DNS.Endpoints)
	}
	// 运行时权威字段：照常以本次操作为准
	if len(saved.Services) != 1 || saved.Services[0] != "github" {
		t.Errorf("服务组切换应落盘，实际 %v", saved.Services)
	}
	if !saved.SystemProxy {
		t.Error("系统代理意图应原样保留")
	}
}

// TestCustomPatternsInjected 自定义站点要在 New 之后就并入匹配集合：
// 托盘/窗口里的分流靠同一个 Set，漏了这一步「加速自己的站点」就等于没开。
func TestCustomPatternsInjected(t *testing.T) {
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Custom = config.CustomConfig{Domains: []string{"my.site.com", "*.example.com"}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, Log: quiet})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	if !rs.Match("my.site.com") || !rs.Match("a.example.com") {
		t.Fatalf("自定义域名未并入匹配集合: %v", rs.CustomPatterns())
	}
}

// TestReloadRules 热加载：换服务组 / 加自定义域名要生效，内容没变则 report「没变化」。
func TestReloadRules(t *testing.T) {
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.MITM.Enabled = false, false
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, ConfigPath: cfgPath, Log: quiet})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	next := config.Default()
	next.SystemProxy, next.MITM.Enabled = false, false
	next.Services = []string{"github"}
	next.Custom = config.CustomConfig{
		Domains:     []string{"my.site.com"},
		MITMDomains: []string{"blocked.site.com"},
	}
	if err := next.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	changed, err := a.ReloadRules(next)
	if err != nil {
		t.Fatalf("ReloadRules: %v", err)
	}
	if !changed {
		t.Fatal("换了服务组与自定义域名，应报告已变化")
	}
	st := a.Status()
	if len(st.Services) != 1 || st.Services[0] != "github" {
		t.Fatalf("服务组未更新: %v", st.Services)
	}
	if !a.rs.Match("blocked.site.com") {
		t.Fatal("自定义反代域名未并入匹配集合")
	}
	if got := a.cfg.MITMPatterns(); len(got) != len(next.MITMPatterns()) {
		t.Fatalf("反代名单未同步: %v vs %v", got, next.MITMPatterns())
	}

	// 同样的内容再来一次：不应再报「已变化」（否则界面每两秒提示一次）
	changed, err = a.ReloadRules(next)
	if err != nil {
		t.Fatalf("ReloadRules(重复): %v", err)
	}
	if changed {
		t.Fatal("内容没变时不应报告已变化")
	}

	// 未知服务组：报错且不改动当前规则
	bad := config.Default()
	bad.Services = []string{"nope"}
	if _, err := a.ReloadRules(bad); err == nil {
		t.Fatal("未知服务组应报错")
	}
	if st := a.Status(); len(st.Services) != 1 || st.Services[0] != "github" {
		t.Fatalf("失败的重新加载不该改动现有规则: %v", st.Services)
	}
}

// TestPauseStateBookkeeping 暂停记录：手动打开某一项后它就不再属于「被暂停」的项。
func TestPauseStateBookkeeping(t *testing.T) {
	var empty *pauseState
	if empty.active() {
		t.Fatal("空记录不应算暂停中")
	}
	if !(&pauseState{sys: true}).active() {
		t.Fatal("有一项被暂停就算暂停中")
	}
	a := &App{paused: &pauseState{sys: true, mitm: true}}
	a.unpauseLocked(true, false)
	if !a.paused.active() || a.paused.sys {
		t.Fatalf("应只清掉系统代理这一项: %+v", a.paused)
	}
	a.unpauseLocked(false, true)
	if a.paused != nil {
		t.Fatalf("全部恢复后应清空暂停记录: %+v", a.paused)
	}
}

// TestStopAllWhenNothingOn 没有任何接管时「一键暂停」是安全的空操作。
func TestStopAllWhenNothingOn(t *testing.T) {
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.MITM.Enabled = false, false
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Options{Config: cfg, Rules: rs, Log: quiet})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	if err := a.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if a.Paused() {
		t.Fatal("没有任何接管时不该进入暂停态")
	}
	if err := a.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// 人为记为暂停中：Status 要如实报告，界面据此把状态词写成「已暂停」。
	a.mu.Lock()
	a.paused = &pauseState{mitm: true}
	a.mu.Unlock()
	if !a.Status().Paused {
		t.Fatal("Status 未报告暂停状态")
	}
}
