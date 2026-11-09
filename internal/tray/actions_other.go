//go:build !windows

package tray

import (
	"log"
	"os/exec"
	"path/filepath"
	"runtime"
)

// openPath 用系统默认程序打开文件或目录。
func openPath(path string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, path).Start()
}

// openInNotepad 在非 Windows 平台退化为默认程序打开。
func openInNotepad(path string) error { return openPath(path) }

// openLocation 在非 Windows 平台退化为打开所在目录。
func openLocation(path string) error { return openPath(filepath.Dir(path)) }

// notifyError 在非 Windows 平台仅记录日志。
func notifyError(title, msg string) { log.Printf("[%s] %s", title, msg) }
