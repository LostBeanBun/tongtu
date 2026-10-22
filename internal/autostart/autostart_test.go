//go:build windows

package autostart

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestCommandLine(t *testing.T) {
	got := CommandLine(`C:\Program Files\Tongtu\tongtu.exe`)
	want := `"C:\Program Files\Tongtu\tongtu.exe" start --hidden`
	if got != want {
		t.Fatalf("命令行 = %q，期望 %q", got, want)
	}
}

func TestXMLTag(t *testing.T) {
	xml := `<?xml version="1.0"?><Task><Actions><Exec><Command>D:\app\tongtu.exe</Command><Arguments>start --hidden</Arguments></Exec></Actions></Task>`
	if got := xmlTag(xml, "Command"); got != `D:\app\tongtu.exe` {
		t.Fatalf("Command = %q", got)
	}
	if got := xmlTag(xml, "Arguments"); got != "start --hidden" {
		t.Fatalf("Arguments = %q", got)
	}
	if got := xmlTag(xml, "Missing"); got != "" {
		t.Fatalf("缺失标签应返回空串，得到 %q", got)
	}
}

// TestRecoverCommand 覆盖断电自愈命令行：带空格的路径必须整体加引号，
// 否则 RunOnce 会把 "C:\Program Files\..." 拆成两个参数而根本起不来。
func TestRecoverCommand(t *testing.T) {
	got := RecoverCommand(`C:\Program Files\Tongtu\tongtu.exe`)
	want := `"C:\Program Files\Tongtu\tongtu.exe" __recover`
	if got != want {
		t.Fatalf("命令行 = %q，期望 %q", got, want)
	}
}

// TestRunValueRoundTrip 覆盖自启值（Run 与 RunOnce 共用的读写实现）的读写与清除。
//
// 用 HKCU 下一个临时键，绝不碰真正的 Run / RunOnce 键：那是用户机器上的真实配置，
// 跑一次测试就改掉它会让「开机自启」凭空变化。
func TestRunValueRoundTrip(t *testing.T) {
	key := fmt.Sprintf(`Software\Tongtu\TestRunValue%d`, time.Now().UnixNano())
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, key) })

	// 键还不存在：读回来是空串，不算错误
	if got, err := readRunValue(key, runOnceValue); err != nil || got != "" {
		t.Fatalf("键不存在时应返回空串，得到 %q（err=%v）", got, err)
	}

	want := RecoverCommand(`C:\Program Files\Tongtu\tongtu.exe`)
	if err := writeRunValue(key, runOnceValue, want); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if got, err := readRunValue(key, runOnceValue); err != nil || got != want {
		t.Fatalf("读回 = %q（err=%v），期望 %q", got, err, want)
	}

	// 幂等写入：值没变时重复写不报错（监护轮询每 1.5 秒会确认一次）
	if err := writeRunValue(key, runOnceValue, want); err != nil {
		t.Fatalf("重复写入失败: %v", err)
	}
	// 改写：例如通了升级换了安装目录
	next := RecoverCommand(`D:\Tools\tongtu.exe`)
	if err := writeRunValue(key, runOnceValue, next); err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if got, _ := readRunValue(key, runOnceValue); got != next {
		t.Fatalf("改写后 = %q，期望 %q", got, next)
	}

	// 清除幂等：优雅退出与 __recover 都可能调它
	if err := deleteRunValue(key, runOnceValue); err != nil {
		t.Fatalf("清除失败: %v", err)
	}
	if err := deleteRunValue(key, runOnceValue); err != nil {
		t.Fatalf("重复清除应无操作: %v", err)
	}
	if got, err := readRunValue(key, runOnceValue); err != nil || got != "" {
		t.Fatalf("清除后应为空，得到 %q（err=%v）", got, err)
	}
}
