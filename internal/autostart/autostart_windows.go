//go:build windows

// Package autostart 管理 Windows 开机自启，提供两种机制：
//   - HKCU Run 键：无需管理员，登录时以当前用户权限启动（仅系统代理模式可用）
//   - 计划任务：需管理员，登录时以最高权限启动（hosts 模式亦可用，且无 UAC 弹窗）
//
// 另外提供「断电自愈」用的 HKCU RunOnce 登记（见 RecoverCommand）：它不是开机自启，
// 只在通途接管期间存在，用来兜底掉电 / 蓝屏后遗留的错误网络配置。
package autostart

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath     = `Software\Microsoft\Windows\CurrentVersion\Run`
	runOnceKeyPath = `Software\Microsoft\Windows\CurrentVersion\RunOnce`
	runValue       = "Tongtu"
	runOnceValue   = "TongtuRecover"
	taskName       = "Tongtu"
	// recoverSubcommand 是「断电自愈」用的隐藏子命令，由 RunOnce 在登录时调用一次。
	recoverSubcommand = "__recover"
)

// Info 描述当前的开机自启配置。
type Info struct {
	RunCmd  string `json:"run_cmd"`  // HKCU Run 中的命令行（空=未设置）
	TaskCmd string `json:"task_cmd"` // 计划任务命令行（空=不存在）
}

// IsAdmin 报告当前进程是否具备管理员权限。
func IsAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// CommandLine 生成自启命令行："<exe>" start --hidden。
func CommandLine(exe string) string {
	return `"` + exe + `" start --hidden`
}

// EnableRun 写入 HKCU Run 自启项（无需管理员）。
func EnableRun(cmdLine string) error {
	return writeRunValue(runKeyPath, runValue, cmdLine)
}

// DisableRun 移除 HKCU Run 自启项（不存在时无操作）。
func DisableRun() error {
	return deleteRunValue(runKeyPath, runValue)
}

// RunCommand 读取 HKCU Run 自启项（不存在返回空串）。
func RunCommand() (string, error) {
	return readRunValue(runKeyPath, runValue)
}

// RecoverCommand 生成「断电自愈」命令行："<exe>" __recover。
//
// 通途接管系统代理 / hosts 期间会在 HKCU\...\RunOnce 里登记这一条：
// 掉电 / 蓝屏 / 强杀这类没机会执行任何代码的退出之后，下次登录由 Windows
// 执行一次，把遗留的系统代理与 hosts 还原掉。优雅退出、暂停或手动关掉接管后
// 会撤掉它（见 cmd/tongtu/recover.go），所以正常情况下系统里不会留下这个条目。
func RecoverCommand(exe string) string {
	return `"` + exe + `" ` + recoverSubcommand
}

// SetRunOnce 登记 RunOnce 自愈项（值已经相同就不写）。
func SetRunOnce(cmdLine string) error {
	return writeRunValue(runOnceKeyPath, runOnceValue, cmdLine)
}

// ClearRunOnce 移除 RunOnce 自愈项（不存在时无操作）。
func ClearRunOnce() error {
	return deleteRunValue(runOnceKeyPath, runOnceValue)
}

// RunOnceCommand 读取 RunOnce 自愈项（不存在返回空串）。
func RunOnceCommand() (string, error) {
	return readRunValue(runOnceKeyPath, runOnceValue)
}

// readRunValue 读取 HKCU 下某个自启键里的字符串值（键或值不存在都返回空串）。
func readRunValue(keyPath, name string) (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// writeRunValue 写入 HKCU 下的自启值（键不存在时创建）。
//
// 值已经相同就跳过写入：监护轮询每 1.5 秒都会确认一次「断电自愈项还在不在」，
// 没必要每次都往注册表里写一遍。
func writeRunValue(keyPath, name, value string) error {
	if cur, err := readRunValue(keyPath, name); err == nil && cur == value {
		return nil
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

// deleteRunValue 删除 HKCU 下的自启值（键或值不存在时无操作）。
func deleteRunValue(keyPath, name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// EnableTask 创建登录自启的计划任务（最高权限；需管理员）。
func EnableTask(cmdLine string) error {
	out, err := run("schtasks", "/Create", "/TN", taskName, "/SC", "ONLOGON",
		"/TR", cmdLine, "/RL", "HIGHEST", "/F")
	if err != nil {
		return fmt.Errorf("创建计划任务失败（需以管理员身份运行）: %v %s", err, strings.TrimSpace(out))
	}
	return nil
}

// DisableTask 删除计划任务（不存在时无操作）。
func DisableTask() error {
	if !taskExists() {
		return nil
	}
	out, err := run("schtasks", "/Delete", "/TN", taskName, "/F")
	if err != nil {
		return fmt.Errorf("删除计划任务失败: %v %s", err, strings.TrimSpace(out))
	}
	return nil
}

// TaskCommand 读取计划任务命令行（不存在返回空串）。使用 XML 输出，避免受系统语言影响。
func TaskCommand() string {
	out, err := run("schtasks", "/Query", "/TN", taskName, "/XML")
	if err != nil {
		return ""
	}
	cmd := strings.TrimSpace(xmlTag(out, "Command"))
	args := strings.TrimSpace(xmlTag(out, "Arguments"))
	return strings.TrimSpace(cmd + " " + args)
}

// Query 读取当前开机自启配置。
func Query() (Info, error) {
	var info Info
	rc, err := RunCommand()
	if err != nil {
		return info, err
	}
	info.RunCmd = rc
	info.TaskCmd = TaskCommand()
	return info, nil
}

func taskExists() bool {
	_, err := run("schtasks", "/Query", "/TN", taskName, "/XML")
	return err == nil
}

// run 执行外部命令并返回合并输出（隐藏窗口，避免闪烁）。
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// xmlTag 提取 XML 中首个 <tag>…</tag> 的内容。
func xmlTag(s, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], close)
	if j < 0 {
		return ""
	}
	return s[i+len(open) : i+j]
}
