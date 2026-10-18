// 通途（Tongtu）：面向境内网络的 Steam / GitHub 加速工具（独立实现，仅供个人学习使用）。
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"tongtu/internal/config"
)

var flagConfig string

var rootCmd = &cobra.Command{
	Use:   "tongtu",
	Short: "通途（Tongtu）— 轻量的 Steam / GitHub 网络加速",
	Long: `通途（Tongtu）只做一件事：在境内网络下稳定访问 Steam 社区与 GitHub 及其附属站点。
  - 本地正向代理：白名单域名经 DoH 干净解析后直连（绕过 DNS 污染）
  - 系统代理模式：接管浏览器流量（无需管理员权限）
  - Hosts 模式：为 Steam 客户端等程序覆盖 hosts（需要管理员权限）
  - 无 SNI 反代：本地解密被封域名、出站剥离 SNI（需先安装本地根证书）

不带任何子命令直接运行（例如双击 tongtu.exe）等同于执行 tongtu start，
启动后所有可配置项都在系统托盘的右键菜单里。

本项目仅供个人学习与技术研究使用，请勿用作其他用途（详见 README 的「使用范围与免责声明」）。
本程序是自由软件，按 GNU GPL v3.0 授权（Copyright (C) 2026 Tongtu authors），
对任何人提供且不附任何担保；第三方组件与致谢见 THIRD-PARTY.md。`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	// 关闭 cobra 内置的「mousetrap」保护。
	// 该保护在检测到进程由资源管理器（即双击 exe）启动时，会打印
	// 「This is a command line tool. / You need to open cmd.exe and run it from there.」
	// 并直接 os.Exit(1)——这会让「双击即开启」完全失效（双击时父进程名正是 explorer.exe）。
	// 通途本就希望双击直接启动（等同 start），因此必须关闭它。回归测试见 root_test.go。
	cobra.MousetrapHelpText = ""
}

// Execute 运行 CLI 并返回退出码。
func Execute() {
	rootCmd.PersistentFlags().StringVar(&flagConfig, "config", "", "配置文件路径（默认 %APPDATA%\\tongtu\\config.yaml）")
	if err := rootCmd.Execute(); err != nil {
		reportFatal(err)
		os.Exit(1)
	}
}

// reportFatal 报告启动/运行失败。
//
// 通途以 GUI 子系统编译：双击启动时没有控制台，往 stderr 打印等于把错误丢进黑洞
// （以前的实现是靠 mousetrap 判断「由资源管理器启动」然后等用户按回车，但那个回车
// 窗根本没有地方显示）。这里改为：有控制台（终端运行 / 输出被重定向）就照常打印，
// 没有就弹一个系统对话框把错误讲清楚。
func reportFatal(err error) {
	msg := err.Error()
	if consoleAvailable() {
		fmt.Fprintln(os.Stderr, "错误:", msg)
		return
	}
	messageBox("通途 Tongtu — 启动失败", msg, mbOK|mbIconError)
}

func loadConfig() (*config.Config, error) {
	return config.Load(flagConfig)
}

// configPath 返回本次生效的配置文件路径（--config 优先，否则默认路径）。
// 用于需要把改动（例如自动换的控制端口）立即写回配置的场景。
func configPath() string {
	if flagConfig != "" {
		return flagConfig
	}
	return config.DefaultPath()
}

// newLogger 按配置创建 slog logger。
//
// 分两路输出，各取所需：
//   - 控制台与日志文件走标准的结构化文本（time/level/msg/k=v，排查问题要的是原始格式）；
//   - 日志窗口走显示层（logview.go）：时间只留时分秒、级别用中文并配色（见 windowHandler）。
func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	writers := []io.Writer{os.Stderr}
	if cfg.Log.File != "" {
		if f, err := openLogFile(cfg.Log.File); err == nil {
			writers = append(writers, f)
		} else {
			fmt.Fprintln(os.Stderr, "警告: 日志文件打开失败:", err)
		}
	}
	opts := &slog.HandlerOptions{Level: level}
	handlers := logHandlers{slog.NewTextHandler(logFanout(writers), opts)}
	if sink := logDisplay(); sink != nil {
		handlers = append(handlers, newWindowHandler(sink, level))
	}
	return slog.New(handlers)
}

// logFanout 把日志同时写向多个目的地，并**忽略单个目的地的写失败**。
//
// 刻意不用 io.MultiWriter：它在第一个出错的目的地上就停下。而通途以 GUI 子系统编译，
// 双击启动时 os.Stderr 正是一个无效句柄（没有控制台），从终端启动又会在界面就绪后
// 主动脱离控制台——这两种情况下若把 os.Stderr 排在前面，日志窗口和日志文件就会一个
// 字都收不到（表现为「日志窗口空白」）。这里逐个写、谁失败都无所谓。
type logFanout []io.Writer

func (f logFanout) Write(p []byte) (int, error) {
	for _, w := range f {
		if w == nil {
			continue
		}
		_, _ = w.Write(p)
	}
	return len(p), nil
}

func openLogFile(p string) (*os.File, error) {
	if !strings.ContainsRune(p, os.PathSeparator) && !strings.Contains(p, "/") {
		p = filepath.Join(config.Dir(), p)
	}
	if err := config.EnsureDir(); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
