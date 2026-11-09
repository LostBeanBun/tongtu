package main

// 窗口里的「待办」：把「还差点什么才算配好」变成可以一键处理的一条条。
//
// 为什么要有它：只会缩在摘要行里的问题（例如系统代理被别的程序顶掉）用户得自己想明白
// 该去哪个菜单处理。待办把「发现问题 → 解决它」放进同一个入口。
//
// 为什么要缓存：判断待办要问系统（自启走计划任务/注册表），都是起进程的开销，
// 不能塞进 3 秒一次的状态刷新里。这里在后台按需刷新，窗口只读缓存（见 runTodoBoard）。

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"tongtu/internal/app"
	"tongtu/internal/autostart"
	"tongtu/internal/config"
	"tongtu/internal/tray"
)

// errTodoCancelled 表示用户在确认框里点了「取消」：不是失败，只是没做。
// 窗口据此给一条灰色提示，而不是当成错误写进日志。
var errTodoCancelled = errors.New("已取消，未做改动")

// todoRefreshEvery 是兜底刷新周期：托盘里的操作也会改变待办，窗口这边定期跟上即可。
const todoRefreshEvery = 30 * time.Second

// todoBoard 缓存当前待办清单（只在后台 goroutine 里计算，窗口线程只读）。
type todoBoard struct {
	mu    sync.Mutex
	items []uiTodoItem
}

var todos todoBoard

// refresh 重新计算待办清单。
func (b *todoBoard) refresh(a *app.App, cfg *config.Config) {
	items := todoItems(a, cfg)
	b.mu.Lock()
	b.items = items
	b.mu.Unlock()
}

// list 返回一份快照（调用方可以随便改，不会影响缓存）。
func (b *todoBoard) list() []uiTodoItem {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uiTodoItem(nil), b.items...)
}

// count 是待办数量（窗口用它决定要不要显示「待办」按钮）。
func (b *todoBoard) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// runTodoBoard 在后台维持待办清单：启动时算一次，之后定时刷新。stop 关闭时退出。
//
// 为什么定时而不是每次状态刷新都算：判断待办要读注册表 / 计划任务（要问系统），
// 30 秒一次的代价可以忽略，3 秒一次就纯属浪费。
func runTodoBoard(a *app.App, cfg *config.Config, stop <-chan struct{}) {
	todos.refresh(a, cfg)
	t := time.NewTicker(todoRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			todos.refresh(a, cfg)
		}
	}
}

// todoItems 计算「还需要用户处理」的事项。
//
// 判定原则：只说**当前配置下**确实没做到的事，并且每一件都能一键解决。
// 因此不列「建议开启某功能」这类产品建议——那是菜单的事，不是待办。
func todoItems(a *app.App, cfg *config.Config) []uiTodoItem {
	return compileTodos(a.Status(), cfg, autoStartEnabled())
}

// compileTodos 是 todoItems 的纯函数内核：状态与外部探测结果都由调用方给，
// 这样「暂停中不该催用户重开」这类规则可以直接用假状态测。
func compileTodos(st app.Status, cfg *config.Config, autostartOn bool) []uiTodoItem {
	var out []uiTodoItem

	// 暂停中「该开却没开」全是用户自己按的，一条都不该报：否则刚点完「暂停」，
	// 界面就催「重新接管系统代理（当前被其他程序占用）」，纯属自己吓自己。
	if !st.Paused {
		// 系统代理被别的程序顶掉了：一键夺回。
		if st.Running && cfg.SystemProxy && !st.SystemProxy {
			out = append(out, uiTodoItem{
				ID:    uiIDTodoTake,
				Label: "重新接管系统代理（当前被其他程序占用）",
			})
		}
	}

	// 开机自启没配：这台机器重启后要手动打开，属于「没配好」。与暂停无关。
	if !autostartOn {
		out = append(out, uiTodoItem{
			ID:    uiIDTodoAuto,
			Label: "开启开机自启（登录后自动启动）",
		})
	}
	return out
}

// autoStartEnabled 报告开机自启是否已经配置（HKCU Run 键或计划任务任一）。
func autoStartEnabled() bool {
	info, err := autostart.Query()
	if err != nil {
		return false
	}
	return info.RunCmd != "" || info.TaskCmd != ""
}

// runTodo 执行一条待办。
func runTodo(a *app.App, cfg *config.Config, id string) error {
	switch id {
	case uiIDTodoAuto:
		return tray.EnableAutostart()
	case uiIDTodoTake:
		return a.SetMode(true)
	}
	return fmt.Errorf("未知待办 %q", id)
}
