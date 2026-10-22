package main

// 待办清单的判断规则：只说「确实没做到、且能一键解决」的事。

import (
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
	cfg.Hosts = true
	cfg.MITM.Enabled = true
	st := app.Status{Running: true, SystemProxy: false, Hosts: false, MITM: false}

	got := compileTodos(st, cfg, false, true, func() bool { return true })
	for _, want := range []string{uiIDTodoTake, uiIDTodoElev, uiIDTodoCert} {
		if !hasID(got, want) {
			t.Errorf("应包含待办 %q，实际 %v", want, statuses(got))
		}
	}
	if hasID(got, uiIDTodoAuto) {
		t.Error("自启已配置时不该再提示自启")
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
	cfg.Hosts = true
	cfg.MITM.Enabled = true
	st := app.Status{Running: true, Paused: true}

	certCalled := false
	got := compileTodos(st, cfg, false, true, func() bool {
		certCalled = true
		return true
	})
	if len(got) != 0 {
		t.Fatalf("暂停中不该有待办，实际 %v", statuses(got))
	}
	if certCalled {
		t.Error("暂停中连证书探测都不该做（没必要起 certutil）")
	}

	if got := compileTodos(st, cfg, false, false, nil); len(got) != 1 || got[0].ID != uiIDTodoAuto {
		t.Fatalf("与暂停无关的自启待办应保留，实际 %v", statuses(got))
	}
}

// TestTodosOnlyWhenRunning 没在跑的时候不报运行期问题（否则「已停止」还催你夺回系统代理）。
// 装证书这类静态准备仍可提示：它不需要加速在跑。
func TestTodosOnlyWhenRunning(t *testing.T) {
	cfg := config.Default()
	cfg.SystemProxy = true
	cfg.Hosts = true
	st := app.Status{Running: false}

	got := compileTodos(st, cfg, false, true, func() bool { return true })
	if hasID(got, uiIDTodoTake) || hasID(got, uiIDTodoElev) {
		t.Fatalf("未运行时不该有运行期待办，实际 %v", statuses(got))
	}
}
