package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"tongtu/internal/app"
	"tongtu/internal/config"
	"tongtu/internal/rules"
)

// TestWindowStatusKey 状态指纹用于「内容没变就不重绘」，因此必须能分辨开关状态。
func TestWindowStatusKey(t *testing.T) {
	base := windowStatus{
		Title:  "通途 Tongtu 0.1.0",
		State:  "运行中",
		Items:  []windowStatusItem{{Label: "系统代理", On: true}},
		Detail: []string{"服务组 steam"},
	}
	same := base
	same.Items = []windowStatusItem{{Label: "系统代理", On: true}}
	if same.key() != base.key() {
		t.Error("内容相同的两份状态指纹应当一致")
	}
	for name, mutate := range map[string]func(s *windowStatus){
		"标题":   func(s *windowStatus) { s.Title = "别的标题" },
		"状态词":  func(s *windowStatus) { s.State = "已停止" },
		"开关":   func(s *windowStatus) { s.Items[0].On = false },
		"开关名":  func(s *windowStatus) { s.Items[0].Label = "代理" },
		"开关条目": func(s *windowStatus) { s.Items = nil },
		"右侧信息": func(s *windowStatus) { s.Detail = []string{"服务组 github"} },
	} {
		s := base
		s.Items = append([]windowStatusItem(nil), base.Items...)
		mutate(&s)
		if s.key() == base.key() {
			t.Errorf("%s 变化后状态指纹不应相同", name)
		}
	}
}

// TestStatusLevel 健康度决定状态词的颜色：期望接管却没接上必须判为降级，
// 否则界面会一律显示「运行中」，把「系统代理没接上」这类问题藏起来。
func TestStatusLevel(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		modes   []modeWish
		want    windowStateLevel
	}{
		{"未运行", false, nil, stateOff},
		{"未运行（即使有期望）", false, []modeWish{{true, false}}, stateOff},
		{"全部生效", true, []modeWish{{true, true}, {true, true}}, stateOK},
		{"没期望也没开（正常）", true, []modeWish{{false, false}, {true, true}}, stateOK},
		{"期望开却没开 → 降级", true, []modeWish{{true, true}, {true, false}}, stateDegraded},
		{"一个降级即降级", true, []modeWish{{false, false}, {true, false}}, stateDegraded},
		{"没有开关时算正常", true, nil, stateOK},
	}
	for _, c := range cases {
		if got := statusLevel(c.running, c.modes...); got != c.want {
			t.Errorf("%s：健康度 = %d，期望 %d", c.name, got, c.want)
		}
	}
}

// TestWindowStatusOfDegraded 走真实代码路径：配置里期望接管系统代理但实际没接上，
// 状态条必须显示「部分生效」，而不是「运行中」。
func TestWindowStatusOfDegraded(t *testing.T) {
	rs, err := rules.Load([]string{"steam"})
	if err != nil {
		t.Fatalf("加载规则: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0" // 随机端口，避免与真实实例冲突

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := app.New(app.Options{Config: cfg, Rules: rs, Log: quiet})
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	// 期望接管系统代理，实际没接管（Options.SysProxy=false）→ 降级
	cfg.SystemProxy = true
	st := windowStatusOf(a, cfg)
	if st.Level != stateDegraded || st.State != "部分生效" {
		t.Errorf("期望接管却没生效时应为「部分生效」（降级），实际 level=%d state=%q", st.Level, st.State)
	}
	if len(st.Items) != 1 || st.Items[0].Label != "系统代理" || st.Items[0].On {
		t.Errorf("状态条开关项应与实际状态一致（系统代理未生效），实际 %+v", st.Items)
	}

	// 不再期望接管 → 全部符合预期，回到「运行中」
	cfg.SystemProxy = false
	st = windowStatusOf(a, cfg)
	if st.Level != stateOK || st.State != "运行中" {
		t.Errorf("全部符合预期时应为「运行中」，实际 level=%d state=%q", st.Level, st.State)
	}
	if st.Title == "" || len(st.Detail) == 0 {
		t.Errorf("标题与第二行信息不应为空: title=%q detail=%v", st.Title, st.Detail)
	}
}

// TestPauseRemainText 定时暂停的剩余时间文案：不足一分钟不说「0 分钟」。
func TestPauseRemainText(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{-1, "不到 1 分钟"},
		{0, "不到 1 分钟"},
		{30, "不到 1 分钟"},
		{60, "1 分钟"},
		{59 * 60, "59 分钟"},
		{3600, "1 小时"},
		{3660, "1 小时 1 分"},
		{2 * 3600, "2 小时"},
		{86400, "1 天"},
	}
	for _, c := range cases {
		if got := pauseRemainText(c.sec); got != c.want {
			t.Errorf("pauseRemainText(%d) = %q，期望 %q", c.sec, got, c.want)
		}
	}
}

// TestHumanUptime 运行时长要写成人话，且只保留一两个量级（状态条一行放不下更多）。
func TestHumanUptime(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{-5, "0 秒"},
		{0, "0 秒"},
		{59, "59 秒"},
		{60, "1 分钟"},
		{3599, "59 分钟"},
		{3600, "1 小时"},
		{3660, "1 小时 1 分"},
		{86399, "23 小时 59 分"},
		{86400, "1 天"},
		{90000, "1 天 1 小时"},
		{3*86400 + 4*3600 + 7*60, "3 天 4 小时"},
	}
	for _, c := range cases {
		if got := humanUptime(c.sec); got != c.want {
			t.Errorf("humanUptime(%d) = %q，期望 %q", c.sec, got, c.want)
		}
	}
}
