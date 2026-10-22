package main

// 日志文件的写入与轮转。
//
// 为什么不引第三方库：需求很小——超过上限就把文件往后挪一位、最旧的丢掉。
// 自己写一遍更容易说清「上限是多少、什么时候轮转、失败会怎样」，
// 而这些恰恰是长期运行（0.4.0 的浸泡测试要跑 24 小时）必须能一眼看懂的部分。

import (
	"fmt"
	"os"
	"sync"
)

const (
	// logMaxBytes 是单个日志文件的上限（默认保留 3 份历史，最坏约 4 倍占用）。
	logMaxBytes = 5 << 20
	// logKeepFiles 是保留的历史份数：tongtu.log.1 / .2 / .3。
	logKeepFiles = 3
)

// rotatingLog 是一个会自我轮转的日志文件，实现 io.Writer。
type rotatingLog struct {
	path string
	max  int64
	keep int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// newRotatingLog 打开（或创建）日志文件；已存在时先按现有大小决定是否立刻轮转。
func newRotatingLog(path string, max int64, keep int) (*rotatingLog, error) {
	if max <= 0 {
		max = logMaxBytes
	}
	if keep < 0 {
		keep = 0
	}
	w := &rotatingLog{path: path, max: max, keep: keep}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w.f = f
	if fi, err := f.Stat(); err == nil {
		w.size = fi.Size()
		if w.size >= w.max {
			w.rotate() // 上次退出时已经写满：这次从新文件开始，而不是先写爆再轮转
		}
	}
	return w, nil
}

// Write 写入日志；会让文件超过上限时先轮转一次。
func (w *rotatingLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil
	}
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 关闭当前文件（幂等）。
func (w *rotatingLog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotate 把当前文件改名成 .1，原有的 .1 顺次往后挪，最旧的版本丢弃。
//
// 轮转过程中的任何失败都不能让日志消失：实在打不开新文件时，退回到继续追加原文件
// （只是不再受上限约束）——宁可占点磁盘，也不要「日志突然不写了」。
func (w *rotatingLog) rotate() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	if w.keep > 0 {
		// .2 → .3、.1 → .2（Windows 上 os.Rename 会覆盖目标，无需先删）
		for i := w.keep - 1; i >= 1; i-- {
			_ = os.Rename(backupName(w.path, i), backupName(w.path, i+1))
		}
		_ = os.Rename(w.path, backupName(w.path, 1))
	} else {
		_ = os.Remove(w.path)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		f, err = os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	}
	if err != nil {
		return // 只能放弃：日志丢了，但程序继续跑
	}
	w.f = f
	w.size = 0
	if fi, err := f.Stat(); err == nil {
		w.size = fi.Size()
	}
}

// backupName 返回第 i 份历史日志的文件名（tongtu.log.1、tongtu.log.2 …）。
func backupName(path string, i int) string {
	return fmt.Sprintf("%s.%d", path, i)
}
