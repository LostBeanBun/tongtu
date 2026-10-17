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

// TestAppLifecycle 验证不接管系统代理/hosts 时的生命周期：
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
	if st.SystemProxy || st.Hosts {
		t.Fatalf("未请求接管时不应开启: sys=%v hosts=%v", st.SystemProxy, st.Hosts)
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
// 切换服务组会更新状态并持久化；不接管系统代理/hosts 时各项开关也能安全写入配置。
func TestRuntimeTogglesPersist(t *testing.T) {
	rs, err := rules.Load([]string{"steam", "github"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.SystemProxy, cfg.Hosts, cfg.MITM.Enabled = false, false, false
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
	if err := a.SetMode(false, false); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新加载配置: %v", err)
	}
	if len(saved.Services) != 1 || saved.Services[0] != "steam" {
		t.Fatalf("服务组未持久化: %v", saved.Services)
	}
	if saved.SystemProxy || saved.Hosts || saved.MITM.Enabled {
		t.Fatalf("开关未持久化: sys=%v hosts=%v mitm=%v",
			saved.SystemProxy, saved.Hosts, saved.MITM.Enabled)
	}
}

// TestOneShotFlagsNotPersisted 一次性启动标志（--no-sysproxy / --no-hosts）不应被
// 其它托盘开关触发的持久化误写进配置。
func TestOneShotFlagsNotPersisted(t *testing.T) {
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default() // 配置里两种模式都是 true
	cfg.Listen = "127.0.0.1:0"
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 模拟 tongtu start --no-sysproxy --no-hosts
	a := New(Options{
		Config: cfg, Rules: rs, ConfigPath: cfgPath, Log: quiet,
		SysProxy: false, HostsMode: false,
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
	if !saved.SystemProxy || !saved.Hosts {
		t.Fatalf("一次性标志被误写入配置: sys=%v hosts=%v", saved.SystemProxy, saved.Hosts)
	}
}
