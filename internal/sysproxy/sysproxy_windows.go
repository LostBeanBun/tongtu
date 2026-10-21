//go:build windows

// Package sysproxy 负责接管/还原 Windows 系统代理设置（HKCU 注册表）。
package sysproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// requiredBypass 是接管期间必须留在「跳过代理」列表里的条目：
// 回环地址与 <local> 不该经本地代理绕一圈。
var requiredBypass = []string{"localhost", "127.0.0.1", "::1", "<local>"}

const regPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// Values 保存系统代理相关的注册表原值（指针区分“不存在”与“零值”）。
type Values struct {
	ProxyEnable   *uint   `json:"proxy_enable,omitempty"`
	ProxyServer   *string `json:"proxy_server,omitempty"`
	ProxyOverride *string `json:"proxy_override,omitempty"`
}

type stateFile struct {
	Saved   Values    `json:"saved"`
	Addr    string    `json:"addr"`
	SavedAt time.Time `json:"saved_at"`
}

var wininet = windows.NewLazySystemDLL("wininet.dll")

func broadcast() {
	// INTERNET_OPTION_SETTINGS_CHANGED / INTERNET_OPTION_REFRESH
	optChanged := wininet.NewProc("InternetSetOptionW")
	optChanged.Call(0, 39, 0, 0)
	optChanged.Call(0, 37, 0, 0)
}

// StatePath 返回崩溃恢复状态文件路径。
func StatePath() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "tongtu", "sysproxy.json")
}

// SavedAddr 返回状态文件里记录的接管地址（上一轮 Enable 写入的代理地址）。
//
// 用来判断这轮接管是否还「活着」：代理端口还连得上，就说明当初接管它的进程仍在运行，
// 这份状态不是崩溃遗留，不该被清理（见 app.RecoverStale）。
func SavedAddr() (string, bool) {
	data, err := os.ReadFile(StatePath())
	if err != nil {
		return "", false
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil || sf.Addr == "" {
		return "", false
	}
	return sf.Addr, true
}

func readValues() (*Values, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.QUERY_VALUE)
	if err != nil {
		return nil, fmt.Errorf("打开注册表 %s: %w", regPath, err)
	}
	defer k.Close()
	v := &Values{}
	if enable, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
		x := uint(enable)
		v.ProxyEnable = &x
	}
	if s, _, err := k.GetStringValue("ProxyServer"); err == nil {
		v.ProxyServer = &s
	}
	if s, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		v.ProxyOverride = &s
	}
	return v, nil
}

func apply(v *Values) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表（需要写权限）: %w", err)
	}
	defer k.Close()

	if v.ProxyEnable != nil {
		if err := k.SetDWordValue("ProxyEnable", uint32(*v.ProxyEnable)); err != nil {
			return err
		}
	} else {
		_ = k.DeleteValue("ProxyEnable")
	}
	if v.ProxyServer != nil {
		if err := k.SetStringValue("ProxyServer", *v.ProxyServer); err != nil {
			return err
		}
	} else {
		_ = k.DeleteValue("ProxyServer")
	}
	if v.ProxyOverride != nil {
		if err := k.SetStringValue("ProxyOverride", *v.ProxyOverride); err != nil {
			return err
		}
	} else {
		_ = k.DeleteValue("ProxyOverride")
	}
	broadcast()
	return nil
}

// writeState 原子地写入状态文件：先写同目录临时文件，再改名覆盖。
//
// 这个文件是「崩溃 / 断电之后还原原值」的唯一依据，绝不能出现半截内容：
// 直接 WriteFile 是「先截断再写」，若正好在这一刻断电或被强杀，留下的是一个空文件
// 或半截 JSON——原值就永远找不回来了（见 Disable 里对损坏文件的兜底）。
func writeState(path string, sf stateFile) error {
	data, err := json.Marshal(sf)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Enable 将系统代理指向 addr，保存原值（同时落盘以便崩溃后恢复）。
func Enable(addr string) (*Values, error) {
	prev, err := readValues()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(StatePath()), 0o755); err != nil {
		return nil, err
	}
	if err := writeState(StatePath(), stateFile{Saved: *prev, Addr: addr, SavedAt: time.Now()}); err != nil {
		return nil, fmt.Errorf("写入代理状态文件: %w", err)
	}
	bypass := mergeBypass(prev.ProxyOverride)
	next := &Values{
		ProxyEnable:   ptr(uint(1)),
		ProxyServer:   ptr(addr),
		ProxyOverride: ptr(bypass),
	}
	if err := apply(next); err != nil {
		// 接管可能只改成功了一部分（例如 ProxyEnable 已置 1、ProxyOverride 写入失败）：
		// 立刻回滚到抓取到的原值，别把系统留在一个半成品状态。
		if rerr := apply(prev); rerr != nil {
			// 连回滚都失败：留着状态文件，退出时（或监护进程）还能凭它还原。
			return nil, fmt.Errorf("设置系统代理失败: %w（回滚也失败: %v，状态文件已保留）", err, rerr)
		}
		_ = os.Remove(StatePath())
		return nil, fmt.Errorf("设置系统代理失败: %w", err)
	}
	return prev, nil
}

// Outcome 说明 Disable 实际做了什么，调用方据此决定日志与提示。
type Outcome int

const (
	// Restored 表示已按接管前保存的原值还原系统代理。
	Restored Outcome = iota
	// NotHeld 表示系统代理已不归我们管（被别的程序接管 / 用户手动改过），注册表未被改动。
	NotHeld
	// DisabledDeadProxy 表示状态文件损坏、原值无法恢复，只把「指向失效本地端口的代理」关掉了：
	// 这是崩溃遗留的典型形态，关掉它至少让系统回到直连，而不是所有请求都撞死代理。
	DisabledDeadProxy
)

// ErrStateCorrupt 表示代理状态文件存在但内容无法解析——多半是断电 / 强杀正好落在写入途中。
// 此时原值已经找不回来，只能做「至少别让网络瘫着」的兜底（见 disableCorruptState）。
var ErrStateCorrupt = errors.New("系统代理状态文件已损坏")

// Disable 恢复系统代理。saved 为 nil 时读取状态文件（用于崩溃恢复），
// 恢复成功后删除状态文件。
//
// 还原前先确认「系统代理仍由通途持有」（注册表里的地址与状态文件记录一致、且代理是开着的）：
// 若中途有别的程序接管了系统代理（Clash / v2rayN 之类），或用户手动关掉了它，这里就**不还原**，
// 否则会把对方的设置覆盖回我们接管前的旧值。返回 NotHeld 报告这种情况，调用方据此提示用户。
func Disable(saved *Values) (Outcome, error) {
	st, serr := readState(StatePath())
	if saved == nil {
		switch {
		case serr == nil:
			saved = &st.Saved
		case os.IsNotExist(serr):
			return NotHeld, nil // 无待恢复状态
		case errors.Is(serr, ErrStateCorrupt):
			return disableCorruptState()
		default:
			return NotHeld, serr
		}
	}
	if st != nil {
		live, lerr := readValues()
		if lerr != nil {
			// 读不到注册表就无从判断，退回「按原值还原」的旧行为
		} else if !heldIn(live, st.Addr) {
			_ = os.Remove(StatePath())
			return NotHeld, nil
		}
	}
	if err := apply(saved); err != nil {
		return NotHeld, fmt.Errorf("还原系统代理失败: %w", err)
	}
	_ = os.Remove(StatePath())
	return Restored, nil
}

// disableCorruptState 处理「状态文件损坏、原值已不可知」的兜底。
//
// 能安全做的只有一件事：如果系统代理此刻指向一个**没人监听的本地端口**（正是通途崩溃后
// 留下的形态），就把代理关掉——否则所有走系统代理的程序（浏览器、商店、更新器）都会
// 卡在「无法连接到代理服务器」。如果端口还有人在听（可能是用户自己的 Clash 等），或者
// 代理指向的是外部地址，就不猜、不动，交给用户按提示检查。
func disableCorruptState() (Outcome, error) {
	live, err := readValues()
	if err != nil {
		return NotHeld, fmt.Errorf("%w：且读不到当前系统代理设置: %v", ErrStateCorrupt, err)
	}
	if !deadLocalProxy(live) {
		return NotHeld, fmt.Errorf("%w：原值已不可恢复，且当前系统代理不像通途留下的（未改动）；"+
			"请检查「设置 → 网络和 Internet → 代理」", ErrStateCorrupt)
	}
	next := &Values{
		ProxyEnable:   ptr(uint(0)),
		ProxyServer:   live.ProxyServer,
		ProxyOverride: live.ProxyOverride,
	}
	if err := apply(next); err != nil {
		return NotHeld, fmt.Errorf("%w：关闭失效的系统代理失败: %v", ErrStateCorrupt, err)
	}
	_ = os.Remove(StatePath())
	return DisabledDeadProxy, nil
}

// deadLocalProxy 报告 v 是否「代理开着 + 指向本机回环地址 + 那个端口没人监听」。
// 这种状态下走系统代理的请求全部失败，是崩溃遗留的典型特征。
func deadLocalProxy(v *Values) bool {
	if v == nil || v.ProxyEnable == nil || *v.ProxyEnable != 1 || v.ProxyServer == nil {
		return false
	}
	addr := strings.TrimSpace(*v.ProxyServer)
	// 多协议写法（http=...;https=...）不猜。
	if addr == "" || strings.ContainsAny(addr, ";=") {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "::1":
	default:
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	conn, derr := net.DialTimeout("tcp", net.JoinHostPort(host, port), 300*time.Millisecond)
	if derr == nil {
		_ = conn.Close()
		return false // 有人监听：可能是用户自己的本地代理，别动
	}
	return true
}

// readState 读取崩溃恢复状态文件。文件不存在时返回 os.ErrNotExist。
func readState(path string) (*stateFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return &st, nil
}

// Held 报告系统代理是否仍由 addr 持有：注册表里代理是开着的，且地址就是 addr。
// 读注册表失败时返回错误（调用方应把它当作「无法判断」，而不是「已被接管」）。
func Held(addr string) (bool, error) {
	v, err := readValues()
	if err != nil {
		return false, err
	}
	return heldIn(v, addr), nil
}

// heldIn 是 Held 的纯函数版本（便于单测）：判断给定注册表值是否仍指向 addr。
func heldIn(v *Values, addr string) bool {
	return v != nil && addr != "" &&
		v.ProxyEnable != nil && *v.ProxyEnable == 1 &&
		v.ProxyServer != nil && *v.ProxyServer == addr
}

// Active 报告是否存在待恢复的代理状态文件。
func Active() bool {
	_, err := os.Stat(StatePath())
	return err == nil
}

// mergeBypass 生成接管期间的「跳过代理」列表：把必须跳过的条目合并进用户原有列表，
// 而不是整段替换。
//
// 用户可能在 ProxyOverride 里配了内网/域名绕过项（如 10.*、*.corp.local）；
// 接管时直接覆盖会把这些流量也塞进本地代理。原值已在 sysproxy.json 里备份、退出时会
// 还原，但**运行期间**同样该保留用户原有的绕过语义。
func mergeBypass(existing *string) string {
	if existing == nil {
		return strings.Join(requiredBypass, ";")
	}
	out := splitBypass(*existing)
	have := make(map[string]bool, len(out)+len(requiredBypass))
	for _, e := range out {
		have[strings.ToLower(e)] = true
	}
	for _, e := range requiredBypass {
		if k := strings.ToLower(e); !have[k] {
			out = append(out, e)
			have[k] = true
		}
	}
	return strings.Join(out, ";")
}

// splitBypass 拆分 ProxyOverride（分号分隔；忽略空白与空项）。
func splitBypass(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ptr[T any](v T) *T { return &v }
