package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"tongtu/internal/config"
)

// TestGuardTokenRoundTrip 覆盖监护令牌的落盘与读回：
// 主进程启动时写入 guard.json（pid + 随机令牌），监护进程据此判断自己还算不算数。
func TestGuardTokenRoundTrip(t *testing.T) {
	path := guardStatePath()
	old, readErr := os.ReadFile(path)
	defer func() {
		if readErr == nil {
			_ = os.WriteFile(path, old, 0o644)
			return
		}
		_ = os.Remove(path)
	}()
	_ = os.Remove(path)

	token := claimGuard()
	if token == "" {
		t.Fatal("claimGuard 应返回令牌")
	}
	length := len(token)
	if length < 16 {
		t.Errorf("令牌太短，容易撞车: %q", token)
	}
	gi, err := readGuardInfo()
	if err != nil {
		t.Fatalf("读取监护令牌失败: %v", err)
	}
	if gi.Token != token || gi.PID != os.Getpid() {
		t.Errorf("令牌文件内容不符: %+v（期望 pid=%d token=%s）", gi, os.Getpid(), token)
	}
	if gi.At.IsZero() {
		t.Error("令牌文件应记录写入时间")
	}

	// 再占一次：令牌必须换新（用来让上一轮的监护进程失效）
	next := claimGuard()
	if next == token {
		t.Error("每次 claimGuard 都应生成新令牌")
	}
}

// TestGuardStillOurs 覆盖「旧监护进程该不该动手」的判断：令牌或 pid 对不上就不许动。
func TestGuardStillOurs(t *testing.T) {
	gi := guardInfo{PID: 4242, Token: "abc", At: time.Now()}
	if !guardStillOurs(gi, 4242, "abc") {
		t.Error("同一轮次（pid 与令牌都一致）应当继续守护")
	}
	if guardStillOurs(gi, 4243, "abc") {
		t.Error("pid 变了说明是新实例，旧监护不能动手")
	}
	if guardStillOurs(gi, 4242, "def") {
		t.Error("令牌变了说明是新实例，旧监护不能动手")
	}
}

// TestForeignGuardOwner 覆盖「遗留清理前的安全检查」：只有当令牌文件里的那个实例不是自己、
// 并且确实还活着时，才认定系统代理 / hosts 由别人持有（此时必须跳过清理）。
func TestForeignGuardOwner(t *testing.T) {
	const self = 1234
	alive := func(pid int) bool { return pid == 9001 }

	if pid, ok := foreignGuardOwner(guardInfo{}, self, alive); ok {
		t.Errorf("没有令牌信息时不该认定有别的实例: pid=%d", pid)
	}
	if _, ok := foreignGuardOwner(guardInfo{PID: self}, self, alive); ok {
		t.Error("自己不该被当成别的实例")
	}
	if _, ok := foreignGuardOwner(guardInfo{PID: 9002}, self, alive); ok {
		t.Error("已经退出的实例不该被当成活的")
	}
	pid, ok := foreignGuardOwner(guardInfo{PID: 9001}, self, alive)
	if !ok || pid != 9001 {
		t.Errorf("活着的另一个实例应当被识别出来: pid=%d ok=%v", pid, ok)
	}
}

// TestIsGuardInvocation 监护进程是同一个 exe 的隐藏子命令，必须能被识别出来，
// 否则它会跟着附加父进程的控制台、进而被终端关闭事件牵连。
func TestIsGuardInvocation(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()

	os.Args = []string{"tongtu.exe"}
	if isGuardInvocation() {
		t.Error("普通启动不应被识别为监护进程")
	}
	os.Args = []string{"tongtu.exe", "start"}
	if isGuardInvocation() {
		t.Error("start 不应被识别为监护进程")
	}
	os.Args = []string{"tongtu.exe", guardSubcommand, "--guard-pid", "1"}
	if !isGuardInvocation() {
		t.Error("__guard 子命令应被识别为监护进程")
	}
	os.Args = []string{"tongtu.exe", "--" + guardSubcommand}
	if !isGuardInvocation() {
		t.Error("--__guard 写法也应被识别")
	}
}

// TestGuardCommandHidden 监护子命令是内部实现细节，不能出现在帮助里。
func TestGuardCommandHidden(t *testing.T) {
	if !guardCmd.Hidden {
		t.Error("__guard 必须是对用户隐藏的子命令")
	}
	if _, err := guardCmd.Flags().GetInt("guard-pid"); err != nil {
		t.Errorf("缺少 --guard-pid: %v", err)
	}
	if _, err := guardCmd.Flags().GetString("guard-token"); err != nil {
		t.Errorf("缺少 --guard-token: %v", err)
	}
	found := false
	for _, c := range rootCmd.Commands() {
		if c == guardCmd {
			found = true
		}
	}
	if !found {
		t.Error("监护子命令应挂在根命令上")
	}
}

// TestGuardLogPathInDataDir 监护日志与其它状态文件放在同一个数据目录，便于排查。
func TestGuardLogPathInDataDir(t *testing.T) {
	if dir := config.Dir(); dir == "" {
		t.Skip("当前环境没有数据目录")
	}
	if !strings.HasPrefix(guardLogPath(), config.Dir()) {
		t.Errorf("监护日志应位于数据目录内: %s", guardLogPath())
	}
}
