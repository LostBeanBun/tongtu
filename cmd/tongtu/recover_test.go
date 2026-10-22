package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"tongtu/internal/autostart"
)

// fakeRunOnce 把 RunOnce 的注册表读写换成内存实现：测试不该改动用户机器上真实的
// RunOnce 键（那会让「登录时自动还原」凭空出现或消失），也不该依赖管理员权限。
type fakeRunOnce struct {
	cur     string
	writes  int
	clears  int
	failSet bool
}

func (f *fakeRunOnce) install(t *testing.T) {
	t.Helper()
	oldGet, oldSet, oldClear := runOnceGet, runOnceSet, runOnceClear
	ra := runOnceArmed.Load()
	runOnceGet = func() (string, error) { return f.cur, nil }
	runOnceSet = func(v string) error {
		if f.failSet {
			return errors.New("注册表写入被拒绝")
		}
		f.cur = v
		f.writes++
		return nil
	}
	runOnceClear = func() error {
		f.cur = ""
		f.clears++
		return nil
	}
	t.Cleanup(func() {
		runOnceGet, runOnceSet, runOnceClear = oldGet, oldSet, oldClear
		runOnceArmed.Store(ra)
	})
}

// TestArmRunOnceIdempotent 覆盖登记自愈项的核心要求：可以每 1.5 秒调一次，
// 但值没变时不许反复写注册表；一旦被系统执行掉（值被删除），下一次巡检必须补回来。
func TestArmRunOnceIdempotent(t *testing.T) {
	f := &fakeRunOnce{}
	f.install(t)
	self, err := os.Executable()
	if err != nil {
		t.Skipf("拿不到自身可执行文件: %v", err)
	}
	want := autostart.RecoverCommand(self)

	armRunOnce(nil)
	if f.cur != want {
		t.Fatalf("登记后 = %q，期望 %q", f.cur, want)
	}
	if f.writes != 1 {
		t.Fatalf("首次登记应写入 1 次，实际 %d", f.writes)
	}
	if !runOnceArmed.Load() {
		t.Error("登记成功后应标记为已武装")
	}

	armRunOnce(nil)
	if f.writes != 1 {
		t.Errorf("值没变时不应重复写入，实际写入 %d 次", f.writes)
	}

	// 断电重启后 Windows 执行过一次 RunOnce，值被系统删掉；巡检要重新登记，
	// 否则这一轮（比如用户手动又开了 hosts）掉电就没人兜底了。
	f.cur = ""
	armRunOnce(nil)
	if f.cur != want || f.writes != 2 {
		t.Errorf("被系统清掉后应补写，cur=%q 写入=%d", f.cur, f.writes)
	}
}

// TestDisarmRunOnceIdempotent 覆盖撤掉自愈项：暂停 / 优雅退出后系统里不许留下常驻命令，
// 并且重复调用不产生多余写入。
func TestDisarmRunOnceIdempotent(t *testing.T) {
	f := &fakeRunOnce{}
	f.install(t)

	// 本来就没有：什么都不做
	disarmRunOnce(nil)
	if f.clears != 0 {
		t.Errorf("没有登记时不应调用清除，实际 %d 次", f.clears)
	}

	armRunOnce(nil)
	disarmRunOnce(nil)
	if f.cur != "" || f.clears != 1 {
		t.Errorf("撤销后 cur=%q 清除=%d，期望空且 1 次", f.cur, f.clears)
	}
	if runOnceArmed.Load() {
		t.Error("撤销后不应再标记为已武装")
	}

	disarmRunOnce(nil)
	if f.clears != 1 {
		t.Errorf("重复撤销不应再写注册表，实际 %d 次", f.clears)
	}
}

// TestArmRunOnceFailureIsHarmless 覆盖注册表写不进去（权限 / 被杀软拦截）的情形：
// 只是这层兜底没了，绝不能影响启动或监护进程。
func TestArmRunOnceFailureIsHarmless(t *testing.T) {
	f := &fakeRunOnce{failSet: true}
	f.install(t)

	armRunOnce(nil)
	if runOnceArmed.Load() {
		t.Error("写入失败时不应标记为已武装（否则日志会谎报已登记）")
	}
}

// TestRecoverCommandHidden 覆盖 __recover 命令本身：它是内部命令，必须隐藏、
// 不接受多余参数，并且与 autostart 生成命令行的子命令名保持一致。
func TestRecoverCommandHidden(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{recoverSubcommand})
	if err != nil {
		t.Fatalf("找不到 %s: %v", recoverSubcommand, err)
	}
	if cmd != recoverCmd {
		t.Fatalf("找到的是 %q，不是 __recover", cmd.Name())
	}
	if !cmd.Hidden {
		t.Error("__recover 必须是隐藏命令（只该由系统登录时调用）")
	}
	if cmd.Args == nil {
		t.Fatal("__recover 应校验参数")
	}
	if err := cmd.Args(cmd, []string{"start"}); err == nil {
		t.Error("__recover 不应接受多余参数")
	}
	// autostart 里也写了一份子命令名（RunOnce 只有一份命令行字符串），防止两处漂移
	if got := autostart.RecoverCommand(`C:\tongtu.exe`); !strings.HasSuffix(got, " "+recoverSubcommand) {
		t.Errorf("RunOnce 命令行 = %q，应以子命令 %s 结尾", got, recoverSubcommand)
	}
}
