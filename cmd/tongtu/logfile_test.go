package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRotatingLogRotatesOnLimit 覆盖「写满就轮转」：当前文件重新从空开始，
// 上一份变成 .1，且不会超过保留份数。
func TestRotatingLogRotatesOnLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tongtu.log")
	w, err := newRotatingLog(path, 100, 2)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	defer w.Close()

	chunk := strings.Repeat("a", 60)
	for i := 0; i < 4; i++ {
		if _, err := w.Write([]byte(chunk + "\n")); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i, err)
		}
	}

	cur := readFileString(t, path)
	if len(cur) > 100 {
		t.Errorf("当前日志文件不该超过上限：%d 字节", len(cur))
	}
	if !strings.Contains(cur, chunk) {
		t.Errorf("最后一次写入应在当前文件里，实际 %q", cur)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("轮转后应存在 .1 备份: %v", err)
	}
	// 只保留 keep=2 份历史：第 3 份不该存在（第 4 次写入会依次推动）
	for _, n := range []int{2, 3, 4} {
		if _, err := os.Stat(backupName(path, n)); err == nil {
			// .2 允许存在（keep=2），.3/.4 不允许
			if n > 2 {
				t.Errorf("%d 份超出保留上限，不该存在 %s", n, backupName(path, n))
			}
		}
	}
}

// TestRotatingLogReopensFullFile 覆盖「启动时文件已经写满」：直接从新文件开始写，
// 而不是先写爆再轮转。
func TestRotatingLogReopensFullFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tongtu.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := newRotatingLog(path, 100, 1)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("新的一行\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if got := readFileString(t, path); got != "新的一行\n" {
		t.Errorf("写满的旧文件应被挪走，实际内容 %q", got)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("旧内容应保留为 .1: %v", err)
	}
}

// TestRotatingLogKeepsWritingWithoutBackup 覆盖「不保留历史（keep=0）」：
// 轮转时直接覆盖原文件，不产生备份。
func TestRotatingLogKeepsWritingWithoutBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tongtu.log")
	w, err := newRotatingLog(path, 10, 0)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	defer w.Close()
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte("0123456789\n")); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	if got := readFileString(t, path); !strings.Contains(got, "0123456789") {
		t.Errorf("轮转后仍要能写入，实际 %q", got)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("keep=0 时不该产生备份文件")
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}
