// Package hosts 负责在系统 hosts 文件中写入/还原由 tongtu 托管的解析块。
package hosts

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tongtu/internal/resolver"
)

const (
	beginMark = "# >>> tongtu managed block >>>"
	endMark   = "# <<< tongtu managed block <<<"
)

// DefaultPath 返回系统 hosts 文件路径。
func DefaultPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "drivers", "etc", "hosts")
	}
	return "/etc/hosts"
}

// Manager 维护 hosts 文件中的托管块。
type Manager struct {
	path     string
	names    []string
	res      *resolver.Resolver
	interval time.Duration
	log      *slog.Logger

	// retryInterval 是刷新失败后的重试间隔（正常时用 interval）。
	retryInterval time.Duration

	local   map[string]bool // 这些主机名强制写 127.0.0.1（本地 MITM 直连）
	localV6 bool            // 本地 IPv6 回环监听就绪时，本地主机名同时写 ::1

	mu sync.Mutex
}

// New 创建 hosts 管理器。names 为需要精确覆盖的主机名列表。
func New(path string, names []string, res *resolver.Resolver, interval time.Duration, log *slog.Logger) *Manager {
	if path == "" {
		path = DefaultPath()
	}
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	return &Manager{
		path:          path,
		names:         append([]string(nil), names...),
		res:           res,
		interval:      interval,
		retryInterval: time.Minute,
		log:           log,
		local:         make(map[string]bool),
	}
}

// SetLocalHosts 设置强制指向本机回环的主机名（本地 MITM 直连场景）。
func (m *Manager) SetLocalHosts(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.local = make(map[string]bool, len(names))
	for _, n := range names {
		if n = normalizeName(n); n != "" {
			m.local[n] = true
		}
	}
}

// SetLocalIPv6 决定本地捕捉的主机名是否同时写 `::1`。
//
// 为什么要写：只写 127.0.0.1 时，域名自己的 AAAA 记录仍然生效，而客户端
// （Windows/Chromium 的 Happy Eyeballs）**优先 IPv6** —— 于是它连的是真实 IPv6 边缘，
// 既不经过本地 :443 捕捉、又可能正好撞上 SNI 封锁，表现为「hosts 模式对这个域名不生效」。
// 写入 `::1` 会把该域名的 AAAA 也指向本地，前提是本地确实监听了 `[::1]`（否则不要写：
// 一个没人监听的 ::1 只会让客户端先失败再回退）。见 app.startLocalMITM。
func (m *Manager) SetLocalIPv6(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.localV6 = on
}

// localIPs 返回本地捕捉主机名要写入的地址列表。
func localIPs(v6 bool) []net.IP {
	if v6 {
		return []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	}
	return []net.IP{net.IPv4(127, 0, 0, 1)}
}

// SetHosts 替换需要覆盖的主机名列表（托盘切换加速服务组时调用）。
// 仅改内存名单，调用方随后应再调用 Apply 才会写入 hosts 文件。
func (m *Manager) SetHosts(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names = append([]string(nil), names...)
}

// Names 返回当前需要覆盖的主机名副本。
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.names...)
}

func normalizeName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	return strings.TrimSuffix(n, ".")
}

// Path 返回 hosts 文件路径。
func (m *Manager) Path() string { return m.path }

// Apply 重新解析名单并写入托管块。返回内容是否发生变化。
func (m *Manager) Apply(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 本地 MITM 主机不解析，直接指向本机回环（IPv6 回环监听就绪时同时写 ::1）
	var resolveNames []string
	for _, n := range m.names {
		if !m.local[n] {
			resolveNames = append(resolveNames, n)
		}
	}
	ips := make(map[string][]net.IP, len(m.names))
	if len(resolveNames) > 0 {
		for h, list := range m.res.ResolveSet(ctx, resolveNames, 8) {
			ips[h] = list
		}
	}
	for n := range m.local {
		ips[n] = localIPs(m.localV6)
	}
	if len(ips) == 0 {
		return false, fmt.Errorf("hosts 名单全部解析失败，已保持现有内容不变")
	}

	content, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsPermission(err) {
			return false, fmt.Errorf("读取 hosts 需要管理员权限: %w", err)
		}
		return false, err
	}
	nl := lineEnding(content)
	text := toLF(string(content))
	block := renderBlock(m.names, ips, nl == "\r\n")
	next, changed := upsertBlock(text, block)
	if !changed {
		return false, nil
	}
	if err := writeHosts(m.path, []byte(fromLF(next, nl))); err != nil {
		if os.IsPermission(err) {
			return false, fmt.Errorf("写入 hosts 需要管理员权限: %w", err)
		}
		return false, err
	}
	m.log.Info("hosts 托管块已更新", "path", m.path, "entries", len(ips))
	m.flushSystemDNS()
	return true, nil
}

// Remove 移除托管块。返回内容是否发生变化。
func (m *Manager) Remove() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	content, err := os.ReadFile(m.path)
	if err != nil {
		return false, err
	}
	if !bytes.Contains(content, []byte(beginMark)) {
		return false, nil
	}
	nl := lineEnding(content)
	text := toLF(string(content))
	next, changed := upsertBlock(text, "")
	if !changed {
		return false, nil
	}
	if err := writeHosts(m.path, []byte(fromLF(next, nl))); err != nil {
		return false, err
	}
	m.log.Info("hosts 托管块已移除", "path", m.path)
	m.flushSystemDNS()
	return true, nil
}

// writeHosts 原子地写入 hosts：先写同目录的临时文件，再改名覆盖。
//
// hosts 是系统级配置，「先清空再写」的过程中一旦被强杀 / 断电，用户会得到一个
// 被截断甚至空白的 hosts（那才是真正会破坏网络的状态）。改名在同一卷上是原子的，
// 最坏情况只留下一个临时文件，原文件始终完整。
// 被系统或安全软件占用而导致改名失败时，退回直接写入（至少不丢功能）。
func writeHosts(path string, data []byte) error {
	tmp := path + ".tongtu.tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		if err := os.Rename(tmp, path); err == nil {
			return nil
		}
		_ = os.Remove(tmp)
	}
	return os.WriteFile(path, data, 0o644)
}

// flushSystemDNS 刷新系统 DNS 缓存（Windows 解析器会缓存 hosts 变更，不刷新则覆盖不生效）。
func (m *Manager) flushSystemDNS() {
	if runtime.GOOS != "windows" {
		return
	}
	if err := exec.Command("ipconfig", "/flushdns").Run(); err != nil {
		m.log.Debug("刷新系统 DNS 缓存失败（不影响直连）", "err", err)
	}
}

// Run 周期性刷新托管块，直到 ctx 结束。
//
// 正常节奏用配置的刷新周期；一旦这一轮失败（DoH 超时、hosts 被占用等）就改用短周期重试，
// 成功后回到正常节奏——不然一次失败会让托管块陈旧整整一个周期。
func (m *Manager) Run(ctx context.Context) {
	retryInterval := m.retryInterval
	if retryInterval <= 0 {
		retryInterval = time.Minute
	}
	wait := m.interval
	if _, err := m.Apply(ctx); err != nil && ctx.Err() == nil {
		m.log.Error("hosts 初始写入失败，稍后重试", "err", err, "重试间隔", retryInterval)
		wait = retryInterval
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if _, err := m.Apply(ctx); err != nil && ctx.Err() == nil {
				m.log.Error("hosts 刷新失败，稍后重试", "err", err, "重试间隔", retryInterval)
				wait = retryInterval
			} else {
				wait = m.interval
			}
			timer.Reset(wait)
		}
	}
}

// renderBlock 生成托管块内容（LF 形式）。
func renderBlock(names []string, ips map[string][]net.IP, crlf bool) string {
	var b strings.Builder
	b.WriteString(beginMark + "\n")
	b.WriteString("# 由 tongtu 自动生成，手动修改会在下次刷新时被覆盖\n")
	for _, name := range names {
		list, ok := ips[name]
		if !ok || len(list) == 0 {
			continue
		}
		for _, ip := range list {
			fmt.Fprintf(&b, "%s\t%s\n", ip, name)
		}
	}
	b.WriteString(endMark + "\n")
	block := b.String()
	if crlf {
		block = strings.ReplaceAll(block, "\n", "\r\n")
	}
	return block
}

// upsertBlock 用 block（可为空，表示删除）替换文本中的托管块。
// 返回结果与是否发生变化。
func upsertBlock(text, block string) (string, bool) {
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		if strings.TrimRight(line, "\r") == beginMark {
			begin = i
		}
		if strings.TrimRight(line, "\r") == endMark && begin >= 0 {
			end = i
			break
		}
	}
	if block == "" {
		if begin < 0 || end < 0 {
			return text, false // 无托管块，无需删除
		}
		rest := append(append([]string{}, lines[:begin]...), lines[end+1:]...)
		out := strings.TrimRight(strings.Join(rest, "\n"), "\n")
		if out != "" {
			out += "\n"
		}
		return out, true
	}
	if begin >= 0 && end >= 0 {
		replaced := append(append([]string{}, lines[:begin]...), strings.Split(strings.TrimRight(block, "\n"), "\n")...)
		replaced = append(replaced, lines[end+1:]...)
		out := strings.Join(replaced, "\n")
		return out, out != text
	}
	// 不存在托管块：追加到文件末尾
	out := strings.TrimRight(text, "\n")
	if out != "" {
		out += "\n\n"
	}
	out += block
	return out, out != text
}

func lineEnding(content []byte) string {
	if bytes.Contains(content, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func toLF(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func fromLF(s, nl string) string {
	if nl == "\r\n" {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}
