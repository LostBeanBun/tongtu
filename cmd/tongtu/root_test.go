package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
)

// errorWriter 总是写失败，用来模拟「GUI 方式启动时无效的 os.Stderr」。
type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("句柄无效") }

// TestLogFanoutSkipsFailedWriter 回归保护：日志要同时写向控制台、日志窗口与日志文件。
// 控制台（os.Stderr）在「双击启动（无控制台）」和「从终端启动后主动脱离控制台」时
// 都是无效句柄——若用 io.MultiWriter（遇到第一个错误即停止），日志窗口与日志文件
// 会一个字都收不到，表现为「双击后日志窗口空白」。
func TestLogFanoutSkipsFailedWriter(t *testing.T) {
	var window, file bytes.Buffer
	f := logFanout{errorWriter{}, &window, &file}
	n, err := f.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v)，期望 (5, nil)：单个目的地失败不应影响整体", n, err)
	}
	if window.String() != "hello" || file.String() != "hello" {
		t.Errorf("前一个目的地写失败不应连累后面的：window=%q file=%q", window.String(), file.String())
	}
}

// TestMousetrapDisabled 回归保护：
// cobra 默认开启 Windows「mousetrap」保护——当进程由资源管理器启动（即双击 exe，
// 父进程名为 explorer.exe）时，会打印「This is a command line tool.」并 os.Exit(1)，
// 使「双击即开启」彻底失效。通途必须关闭它。
func TestMousetrapDisabled(t *testing.T) {
	if cobra.MousetrapHelpText != "" {
		t.Fatalf("必须关闭 cobra mousetrap，当前值 = %q", cobra.MousetrapHelpText)
	}
}

// TestNoArgsDefaultsToStart 无子命令（双击）时根命令应可直接运行，且等同 start。
func TestNoArgsDefaultsToStart(t *testing.T) {
	if rootCmd.RunE == nil {
		t.Fatal("根命令应设置 RunE（无参数即 start）")
	}
	if startCmd.RunE == nil {
		t.Fatal("start 子命令应有 RunE")
	}
	if rootCmd.Args == nil {
		t.Fatal("根命令应校验参数（Args 不为 nil）")
	}
}

// TestStartFlags 启动开关挂在根命令的持久标志上：tongtu --hidden 与
// tongtu start --hidden 都要能生效（开机自启用的就是根命令这条路径）。
func TestStartFlags(t *testing.T) {
	for _, name := range []string{"no-tray", "no-sysproxy", "no-hosts", "no-mitm", "hidden"} {
		f := rootCmd.PersistentFlags().Lookup(name)
		if f == nil {
			t.Errorf("缺少启动开关 --%s", name)
			continue
		}
		if startCmd.Flags().Lookup(name) == nil && startCmd.InheritedFlags().Lookup(name) == nil {
			t.Errorf("start 子命令继承不到 --%s", name)
		}
	}
}

// TestStatusWord 三种状态分开写：暂停中的实例还活着，不该显示成「已停止」。
func TestStatusWord(t *testing.T) {
	cases := []struct {
		name string
		st   app.Status
		want string
	}{
		{"停止", app.Status{}, "已停止"},
		{"运行", app.Status{Running: true, UptimeSec: 90}, "运行中 (1 分 30 秒)"},
		{"暂停", app.Status{Running: true, Paused: true, UptimeSec: 3660}, "已暂停 (1 时 1 分，可恢复"},
	}
	for _, c := range cases {
		got := statusWord(&c.st)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("%s：状态词 %q，期望以 %q 开头", c.name, got, c.want)
		}
	}
	if got := statusWord(&app.Status{Running: true, Paused: true}); !strings.Contains(got, "可恢复") {
		t.Errorf("暂停时应说明怎么恢复：%q", got)
	}
}
