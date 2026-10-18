package main

import (
	"encoding/json"
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

// stashGuardState 备份/恢复 guard.json，避免这些用例污染真实数据目录。
func stashGuardState(t *testing.T) func() {
	t.Helper()
	path := guardStatePath()
	old, err := os.ReadFile(path)
	return func() {
		if err == nil {
			_ = os.WriteFile(path, old, 0o644)
			return
		}
		_ = os.Remove(path)
	}
}

// writeGuardState 写入一份指定的令牌文件。
func writeGuardState(t *testing.T, gi guardInfo) {
	t.Helper()
	data, err := json.Marshal(gi)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guardStatePath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setGuardToken 替换「本轮令牌」（进程内全局状态），返回还原函数。
func setGuardToken(t *testing.T, token string) func() {
	t.Helper()
	guard.mu.Lock()
	old := guard.token
	guard.token = token
	guard.mu.Unlock()
	return func() {
		guard.mu.Lock()
		guard.token = old
		guard.mu.Unlock()
	}
}

// TestRecycleStaleGuard 覆盖「上一轮监护进程没活到回收」时的残留清理：
// 只有令牌的主人确实已经退出时才删——还活着的实例、或自己的令牌都不能动。
func TestRecycleStaleGuard(t *testing.T) {
	restore := stashGuardState(t)
	defer restore()

	// 主人已退出（用一个不可能存在的 pid）
	writeGuardState(t, guardInfo{PID: 1 << 30, Token: "stale", At: time.Now()})
	if !recycleStaleGuard() {
		t.Error("主人已退出的陈旧令牌应被回收")
	}
	if _, err := os.Stat(guardStatePath()); !os.IsNotExist(err) {
		t.Error("陈旧令牌文件应被删除")
	}
	// 重复调用：没有文件时安静返回 false
	if recycleStaleGuard() {
		t.Error("没有令牌文件时不该报告回收成功")
	}

	// 就是自己的令牌（当前进程正在用），绝不能删
	writeGuardState(t, guardInfo{PID: os.Getpid(), Token: "mine", At: time.Now()})
	if recycleStaleGuard() {
		t.Error("自己的令牌不该被回收")
	}
	if _, err := os.Stat(guardStatePath()); err != nil {
		t.Errorf("自己的令牌文件不该被删除: %v", err)
	}

	// 另一个活着的实例（这里借自己当替身：pid 有效且存活）也不该被删
	writeGuardState(t, guardInfo{PID: os.Getppid(), Token: "alive", At: time.Now()})
	_ = recycleStaleGuard()
	if _, err := os.Stat(guardStatePath()); err != nil {
		t.Errorf("进程仍存活时不该回收: %v", err)
	}
}

// TestReleaseGuardOnlyOwnToken 覆盖优雅退出时的令牌回收：只能删属于自己的那一轮。
//
// 这个约束是从「退出后立刻重启」推出来的：新实例启动时会先写下新令牌，
// 旧实例此刻若把文件删掉，新实例的监护进程会读到空文件而放弃兜底还原。
func TestReleaseGuardOnlyOwnToken(t *testing.T) {
	restore := stashGuardState(t)
	defer restore()
	restoreToken := setGuardToken(t, "own-token")
	defer restoreToken()

	// 自己的令牌：回收
	setGuardToken(t, "own-token")
	writeGuardState(t, guardInfo{PID: os.Getpid(), Token: "own-token", At: time.Now()})
	releaseGuard()
	if _, err := os.Stat(guardStatePath()); !os.IsNotExist(err) {
		t.Error("本轮令牌应在优雅退出时被回收")
	}

	// 新实例已经写下新令牌（pid 还是自己也没关系，令牌不同就说明换轮次了）：不能删
	setGuardToken(t, "own-token")
	writeGuardState(t, guardInfo{PID: os.Getpid(), Token: "new-round", At: time.Now()})
	releaseGuard()
	if gi, err := readGuardInfo(); err != nil || gi.Token != "new-round" {
		t.Errorf("不该删掉新实例的令牌: %+v err=%v", gi, err)
	}

	// 令牌文件属于另一个进程：更不能删
	setGuardToken(t, "own-token")
	writeGuardState(t, guardInfo{PID: os.Getpid() + 1, Token: "own-token", At: time.Now()})
	releaseGuard()
	if _, err := os.Stat(guardStatePath()); err != nil {
		t.Errorf("别人的令牌不该被删除: %v", err)
	}

	// 本轮没有令牌（例如没走到 claimGuard）：什么都不做
	setGuardToken(t, "")
	_ = os.Remove(guardStatePath())
	releaseGuard()
	if _, err := os.Stat(guardStatePath()); !os.IsNotExist(err) {
		t.Error("没有本轮令牌时不该创建文件")
	}
}

// TestMarkBreakawayFailed 覆盖「监护进程无法脱离作业对象」的标记：
// 必须能被 guardRow 读到，才能在启动摘要里如实告警。
func TestMarkBreakawayFailed(t *testing.T) {
	guard.mu.Lock()
	old := guard.breakawayFailed
	guard.breakawayFailed = false
	guard.mu.Unlock()
	defer func() {
		guard.mu.Lock()
		guard.breakawayFailed = old
		guard.mu.Unlock()
	}()

	if guardBreakawayFailed() {
		t.Fatal("初始状态应为 false")
	}
	guard.markBreakawayFailed()
	if !guardBreakawayFailed() {
		t.Error("标记后应为 true")
	}
}

// TestGuardRow 覆盖「崩溃防护」摘要行：一切正常时不出行，受限时必须以警告呈现，
// 否则用户会以为有监护进程就万无一失。
func TestGuardRow(t *testing.T) {
	if _, ok := guardRow(false); ok {
		t.Error("一切正常时不该出现崩溃防护行（没消息就是好消息）")
	}
	row, ok := guardRow(true)
	if !ok {
		t.Fatal("无法脱离作业对象时应出现崩溃防护行")
	}
	if row.Mark != markWarn {
		t.Errorf("受限应标为警告标记: %v", row.Mark)
	}
	if row.Label != "崩溃防护" {
		t.Errorf("标签不对: %q", row.Label)
	}
	if !strings.Contains(row.Value, "受限") {
		t.Errorf("说明里要写清是「受限」: %q", row.Value)
	}
}
