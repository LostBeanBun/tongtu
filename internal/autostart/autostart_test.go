//go:build windows

package autostart

import "testing"

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
