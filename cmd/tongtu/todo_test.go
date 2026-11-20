package main

// 待办清单的判断规则：只说「确实没做到、且能一键解决」的事。

import (
	"strings"
	"testing"

	"tongtu/internal/app"
	"tongtu/internal/config"
)

func statuses(list []uiTodoItem) []string {
	out := make([]string, 0, len(list))
	for _, it := range list {
		out = append(out, it.ID)
	}
	return out
}

func hasID(list []uiTodoItem, id string) bool {
	for _, it := range list {
		if it.ID == id {
			return true
		}
	}
	return false
}

// TestTodosRuntimeFix 想要的没做到：提出待办。
func TestTodosRuntimeFix(t *testing.T) {
	cfg := config.Default()
	cfg.SystemProxy = true
	cfg.MITM.Enabled = true
	st := app.Status{Running: true, WantSystemProxy: true, SystemProxy: false, MITM: false}

	got := compileTodos(st, cfg, true, func() bool { return true })
	for _, want := range []string{uiIDTodoTake, uiIDTodoCert} {
		if !hasID(got, want) {
			t.Errorf("应包含待办 %q，实际 %v", want, statuses(got))
		}
	}
	if hasID(got, uiIDTodoAuto) {
		t.Error("自启已配置时不该再提示自启")
	}
}

// TestTodosNoSysproxyIsSilent 【中4】配置里要开系统代理、但本轮意图就是不接管
// （--no-sysproxy 启动）：一条「重新接管」待办都不该出现——用户刚明确说过不要，
// 界面却催他「夺回被占用的系统代理」，纯属误报。
func TestTodosNoSysproxyIsSilent(t *testing.T) {
	cfg := config.Default()
	cfg.SystemProxy = true // 配置文件仍写着「要开」（一次性启动标志不写回配置）
	st := app.Status{Running: true, WantSystemProxy: false, SystemProxy: false}

	got := compileTodos(st, cfg, true, func() bool { return true })
	if hasID(got, uiIDTodoTake) {
		t.Fatalf("意图不接管时不该报「重新接管」待办，实际 %v", statuses(got))
	}
}

// TestTodosTakeLabelAttribution 「重新接管」的原因要说实话：
// 有过接管、后来丢了才叫「被占用」；接管从没成功过只能说「重试」。
func TestTodosTakeLabelAttribution(t *testing.T) {
	cfg := config.Default()
	cfg.SystemProxy = true

	labelOf := func(st app.Status) string {
		for _, it := range compileTodos(st, cfg, true, nil) {
			if it.ID == uiIDTodoTake {
				return it.Label
			}
		}
		return ""
	}

	// 接管过、被别的程序顶掉（Status 只在真被顶掉时才写备注）
	stolen := labelOf(app.Status{
		Running: true, WantSystemProxy: true, SystemProxy: false,
		ProxyNote: "已被其他程序接管",
	})
	if !strings.Contains(stolen, "已被其他程序接管") {
		t.Errorf("真被顶掉时应说明「被接管」，实际 %q", stolen)
	}

	// 从没接管成功（启动时注册表写失败等）：没有被顶掉的事实，不能说「被占用」
	fresh := labelOf(app.Status{Running: true, WantSystemProxy: true, SystemProxy: false})
	if fresh == "" {
		t.Fatal("接管未生效也应有可一键重试的待办")
	}
	if strings.Contains(fresh, "占用") || strings.Contains(fresh, "接管系统代理（当前已被") {
		t.Errorf("没被顶掉过不该说「被其他程序占用」，实际 %q", fresh)
	}
}

// TestTodosPausedIsSilent 用户主动暂停后，一条「该开却关着」的提示都不该出现。
//
// 这是真机验收发现的问题：点完「暂停」，待办从 1 变成 2，多出来的是
// 「重新接管系统代理（当前被其他程序占用）」——用户刚亲手关掉的东西，
// 界面却当成故障催他修。
func TestTodosPausedIsSilent(t *testing.T) {
	cfg := config.Default()
	cfg.SystemProxy = true
	cfg.MITM.Enabled = true
	st := app.Status{Running: true, WantSystemProxy: true, Paused: true}

	certCalled := false
	got := compileTodos(st, cfg, true, func() bool {
		certCalled = true
		return true
	})
	if len(got) != 0 {
		t.Fatalf("暂停中不该有待办，实际 %v", statuses(got))
	}
	if certCalled {
		t.Error("暂停中连证书探测都不该做（没必要起 certutil）")
	}

	if got := compileTodos(st, cfg, false, nil); len(got) != 1 || got[0].ID != uiIDTodoAuto {
		t.Fatalf("与暂停无关的自启待办应保留，实际 %v", statuses(got))
	}
}

// TestTodosOnlyWhenRunning 没在跑的时候不报运行期问题（否则「已停止」还催你夺回系统代理）。
// 装证书这类静态准备仍可提示：它不需要加速在跑。
func TestTodosOnlyWhenRunning(t *testing.T) {
	cfg := config.Default()
	cfg.SystemProxy = true
	st := app.Status{Running: false, WantSystemProxy: true}

	got := compileTodos(st, cfg, true, func() bool { return true })
	if hasID(got, uiIDTodoTake) {
		t.Fatalf("未运行时不该有运行期待办，实际 %v", statuses(got))
	}
}
