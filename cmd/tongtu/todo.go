package main

// 窗口里的「待办」：把「还差点什么才算配好」变成可以一键处理的一条条。
//
// 为什么要有它：0.1.x 里「hosts 没生效」「证书没装」只会缩在摘要行里，用户得自己
// 想明白该去哪个菜单、执行哪条命令。待办把「发现问题 → 解决它」放进同一个入口。
//
// 为什么要缓存：判断待办要问系统（证书存储走 certutil、自启走 schtasks），都是起进程的
// 开销，不能塞进 3 秒一次的状态刷新里。这里在后台按需刷新，窗口只读缓存（见 runTodoBoard）。

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"tongtu/internal/app"
	"tongtu/internal/autostart"
	"tongtu/internal/config"
	"tongtu/internal/mitm"
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
// 为什么定时而不是每次状态刷新都算：判断待办要问证书存储与计划任务（起进程），
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
	return compileTodos(a.Status(), cfg, autostart.IsAdmin(), autoStartEnabled(), func() bool {
		ok, err := mitm.CertInstalled(a.DataDir())
		return err == nil && !ok
	})
}

// compileTodos 是 todoItems 的纯函数内核：状态与几个外部探测结果都由调用方给，
// 这样「暂停中不该催用户重开」这类规则可以直接用假状态测。
func compileTodos(st app.Status, cfg *config.Config, admin, autostartOn bool, certMissing func() bool) []uiTodoItem {
	var out []uiTodoItem

	// 暂停中「该开却没开」全是用户自己按的，一条都不该报：否则刚点完「暂停」，
	// 界面就催「重新接管系统代理（当前被其他程序占用）」，纯属自己吓自己。
	if !st.Paused {
		// 配了要开反代，但反代没起来、且证书确实没装：装证书就能解决。
		// （反代没起来也可能是别的原因，那种情况不在这里假装能一键修好。）
		if cfg.MITM.Enabled && !st.MITM && certMissing != nil && certMissing() {
			out = append(out, uiTodoItem{
				ID:    uiIDTodoCert,
				Label: "安装本地根证书（无 SNI 反代需要）",
			})
		}

		// 想要 hosts 模式，但当前不是管理员：只能提权重启。
		if st.Running && cfg.Hosts && !st.Hosts && !admin {
			out = append(out, uiTodoItem{
				ID:    uiIDTodoElev,
				Label: "以管理员身份重启（让 Hosts 模式生效）",
			})
		}

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

// runTodo 执行一条待办；restartElevated 由启动流程注入（它会先确认，再请求优雅退出重启）。
func runTodo(a *app.App, cfg *config.Config, id string, restartElevated func() bool) error {
	switch id {
	case uiIDTodoCert:
		if err := mitm.InstallIfNeeded(a.DataDir()); err != nil {
			return err
		}
		// 装证书的动机就是要用反代：顺手打开，省掉「再点一次开关」。
		if st := a.Status(); st.Running && !st.MITM {
			if err := a.SetMITM(true); err != nil {
				return fmt.Errorf("证书已安装，但开启无 SNI 反代失败: %w", err)
			}
		}
		return nil
	case uiIDTodoElev:
		if restartElevated == nil {
			return fmt.Errorf("当前启动方式不支持自动提权重启，请右键托盘图标选择「以管理员身份重启」")
		}
		if !restartElevated() {
			return errTodoCancelled
		}
		return nil
	case uiIDTodoAuto:
		return tray.EnableAutostart(false)
	case uiIDTodoTake:
		st := a.Status()
		return a.SetMode(true, st.Hosts)
	}
	return fmt.Errorf("未知待办 %q", id)
}
