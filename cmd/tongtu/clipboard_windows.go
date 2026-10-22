//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// copyToClipboard 把文本放进剪贴板（CF_UNICODETEXT）。
//
// 为什么不直接调 Win32 的 OpenClipboard / GlobalAlloc：那条路要自己做
// uintptr→unsafe.Pointer 的内存搬运（go vet 会判为 unsafe 误用），长度也算错就截断。
// 「复制诊断」是低频手动操作，起一次 PowerShell（约 0.3 秒）换来的简单与正确更划算。
// 文本先落到临时文件再读，避免把任意内容拼进命令行造成转义问题。
func copyToClipboard(text string) error {
	f, err := os.CreateTemp("", "tongtu-clip-*.txt")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return fmt.Errorf("写临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("写临时文件失败: %w", err)
	}

	quoted := strings.ReplaceAll(path, "'", "''")
	script := "[System.IO.File]::ReadAllText('" + quoted +
		"', [System.Text.Encoding]::UTF8) | Set-Clipboard"
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("写入剪贴板失败: %s", msg)
	}
	return nil
}
