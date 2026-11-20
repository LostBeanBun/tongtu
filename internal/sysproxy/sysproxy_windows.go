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
	"sync"
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

// opMu 串行化 Enable / Disable / Retarget / Held：健康自愈可能在退出流程读取或改写
// 同一份注册表与状态文件，交错执行会留下「注册表指向新端口、状态文件还是旧端口」的半成品。
var opMu sync.Mutex

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
	opMu.Lock()
	defer opMu.Unlock()
	prev, err := readValues()
	if err != nil {
		return nil, err
	}
	// 不抢另一个**仍活着**的通途实例的接管：状态文件记录的地址不是本轮地址、
	// 注册表确实指向它、而且那个地址还在监听（监听者就是建立那轮接管的实例）。
	// 抢过来会把它的原值快照覆盖成它的地址——它退出时校验不过（NotHeld）只能干瞪眼，
	// 我这轮退出时又会把系统代理还原到它已经死掉的地址上：两边都还原不回真正的原值。
	// 启动路径与托盘「重新接管」共用这里，统一拒绝并把原因说清楚。
	if st, rerr := readState(StatePath()); rerr == nil &&
		peerHoldsSystemProxy(prev, st, addr, addrListening) {
		return nil, fmt.Errorf("%w（代理地址 %s 仍在监听，多半是先启动的那个实例）", ErrHeldByPeer, st.Addr)
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

// ErrHeldByPeer 表示系统代理此刻正被另一个仍活着的通途实例持有：本轮不接管。
// 两个实例先后接管会互相覆盖状态文件，双方退出时都还原不回真正的原值，
// 最后一次还原可能把系统代理留在无人监听的地址上（详见 Enable 的注释）。
var ErrHeldByPeer = errors.New("系统代理正被另一个仍在运行的通途实例持有")

// Disable 恢复系统代理。
//
//	saved：接管前的原值（本轮 Enable 的返回值）；nil 表示崩溃恢复——原值与接管地址
//	都取自状态文件，行为与历史版本一致（校验、清理均以文件记录为准）。
//	heldAddr：调用方**本轮接管的代理地址**（saved 非 nil 时参与校验）。
//
// 还原前先确认「系统代理仍归本轮持有」：注册表里的地址必须仍指向 heldAddr。
// **不拿状态文件做校验凭据**——多实例下它可能已被下一个接管的实例覆盖，
// 拿它校验会让先退出的实例还原掉对方的接管、并删掉对方的原值记录（见 planDisable）。
// 校验不过返回 NotHeld，不动注册表；状态文件去留按 planDisable 的归属规则处理。
func Disable(saved *Values, heldAddr string) (Outcome, error) {
	opMu.Lock()
	defer opMu.Unlock()
	st, serr := readState(StatePath())
	if saved == nil {
		// 崩溃恢复：调用方没有任何凭据，唯一真相就是状态文件
		//（接管地址也以文件为准，后续校验语义与历史版本完全一致）。
		switch {
		case serr == nil:
			saved, heldAddr = &st.Saved, st.Addr
		case os.IsNotExist(serr):
			return NotHeld, nil // 无待恢复状态
		case errors.Is(serr, ErrStateCorrupt):
			return disableCorruptState()
		default:
			return NotHeld, serr
		}
	}
	if serr != nil {
		// 文件读不出来（损坏 / 权限）但调用方自带原值：按「无文件」处理——
		// 校验只依赖 heldAddr，不依赖文件。
		st = nil
	}
	live, lerr := readValues()
	act := planDisable(st, saved, heldAddr, live, lerr == nil)
	if act.next != nil {
		if err := apply(act.next); err != nil {
			return NotHeld, fmt.Errorf("还原系统代理失败: %w", err)
		}
	}
	if act.removeState {
		_ = os.Remove(StatePath())
	}
	return act.outcome, nil
}

// disableAction 是 planDisable 的结论：给 Disable 的三个执行动作。
type disableAction struct {
	outcome     Outcome
	next        *Values // nil = 不动注册表
	removeState bool
}

// planDisable 是 Disable 的决策内核（纯函数，仿 retargetPlan，见测试）。
//
// heldAddr 是**本轮接管地址**：只有注册表仍指向它，才轮到本调用方还原。
// 状态文件属于哪一轮**不作凭据**——典型多实例链：
//
//	A 接管（文件: 原A/addrA）→ B 接管（文件被覆盖: addrA/addrB）→ A 先退出。
//
// 若拿文件记录（addrB）校验，A 会「校验通过」从而还原掉 B 的接管并删掉文件；
// B 后退出时没有文件可校验，直接把系统代理还原到 A 已经死掉的地址上，
// 而监护 / RunOnce 两条兜底此时都被正常退出流程撤掉——死代理从此无人收拾。
// 用 heldAddr 校验后：A 校验不过（NotHeld，文件留给 B），B 仍持有则正常还原。
func planDisable(st *stateFile, saved *Values, heldAddr string, live *Values, liveOK bool) disableAction {
	// 注册表读不到：无从校验，退回「按原值还原」的旧行为（宁可还原也不留半吊子状态）。
	if !liveOK {
		return disableAction{outcome: Restored, next: saved, removeState: st != nil && st.Addr == heldAddr}
	}
	if !heldIn(live, heldAddr) {
		// 不归本轮管：被其他程序接管、用户手动改过、或另一个实例换了自己的地址。
		// 注册表一动不动；状态文件只在「记录的接管已不在注册表里」时清掉——
		// 那是孤儿记录（其主人显然没在持有），留着只会让每次启动都误报一次；
		// 记录的正是注册表当前值时必须留：那是另一个存活实例退出时唯一的原值凭据。
		return disableAction{outcome: NotHeld, removeState: st != nil && !heldIn(live, st.Addr)}
	}
	// 本轮确实持有：按原值还原。文件只有记录的就是本轮接管时才删
	//（崩溃恢复路径的 heldAddr 就取自文件，恒成立；记录着别人轮次的文件不归我们删）。
	return disableAction{
		outcome:     Restored,
		next:        saved,
		removeState: st != nil && st.Addr == heldAddr,
	}
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

// Retarget 在代理监听地址变化时，把系统代理从旧地址改到 addr。
//
// 只在「系统代理仍由本实例持有」时动手：被别的程序接管后这里什么都不做（返回 false），
// 继续遵守「不抢也不覆盖」的承诺。状态文件里的 Addr 同步更新，接管前的原值保持不变。
func Retarget(addr string) (bool, error) {
	opMu.Lock()
	defer opMu.Unlock()
	if addr == "" {
		return false, nil
	}
	st, err := readState(StatePath())
	if err != nil {
		// 没有状态文件 = 这轮接管不是我们建立的，不改注册表。
		return false, nil
	}
	live, err := readValues()
	if err != nil {
		return false, err
	}
	next, changed := retargetPlan(st, live, addr)
	if !changed {
		return false, nil
	}

	oldAddr := st.Addr
	st.Addr = addr
	if err := writeState(StatePath(), *st); err != nil {
		return false, fmt.Errorf("更新代理状态文件失败: %w", err)
	}
	if err := apply(next); err != nil {
		// 回滚状态文件与注册表，别留下「状态文件指向新端口、注册表还是旧端口」的半成品。
		st.Addr = oldAddr
		_ = writeState(StatePath(), *st)
		_ = apply(live)
		return false, fmt.Errorf("更新系统代理地址失败: %w", err)
	}
	return true, nil
}

// retargetPlan 计算换口后要写入的注册表值（纯函数，便于单测）。
// 返回 nil, false 表示「不该动」：没有状态、已不归我们持有，或地址本来就没变。
func retargetPlan(st *stateFile, live *Values, addr string) (*Values, bool) {
	if st == nil || live == nil || addr == "" || st.Addr == addr {
		return nil, false
	}
	if !heldIn(live, st.Addr) {
		return nil, false
	}
	next := *live
	next.ProxyServer = ptr(addr)
	return &next, true
}

// Held 报告系统代理是否仍由 addr 持有：注册表里代理是开着的，且地址就是 addr。
// 读注册表失败时返回错误（调用方应把它当作「无法判断」，而不是「已被接管」）。
func Held(addr string) (bool, error) {
	opMu.Lock()
	defer opMu.Unlock()
	v, err := readValues()
	if err != nil {
		return false, err
	}
	return heldIn(v, addr), nil
}

// LiveState 返回系统代理的当前注册表原值，供调用方给「没生效」归因
// （被别的程序顶掉 / 被关掉 / 读不出状态），而不是一律猜「被接管」。
// 与 Held 同一把锁，读失败时返回错误（调用方按「无法判断」处理）。
func LiveState() (*Values, error) {
	opMu.Lock()
	defer opMu.Unlock()
	return readValues()
}

// heldIn 是 Held 的纯函数版本（便于单测）：判断给定注册表值是否仍指向 addr。
func heldIn(v *Values, addr string) bool {
	return v != nil && addr != "" &&
		v.ProxyEnable != nil && *v.ProxyEnable == 1 &&
		v.ProxyServer != nil && *v.ProxyServer == addr
}

// peerHoldsSystemProxy 报告「注册表此刻是否正被另一个仍活着的通途实例持有」
// （纯函数，监听探测由调用方注入，便于单测）。三个条件缺一不可：
//
//   - 状态文件记录的接管地址不是本轮地址（自己不可能持有自己还没建立的接管）；
//   - 注册表确实指向那个地址（那轮接管仍生效——不在注册表里就不算有人持有）；
//   - 那个地址还在监听（监听者即建立接管的实例；地址死了就是遗留状态，
//     该按正常接管处理，由启动时的 app.RecoverStale 负责清理）。
func peerHoldsSystemProxy(live *Values, st *stateFile, selfAddr string, listening func(string) bool) bool {
	return st != nil && st.Addr != "" && st.Addr != selfAddr &&
		heldIn(live, st.Addr) && listening(st.Addr)
}

// addrListening 报告 addr 上是否已有进程在监听（300ms 上限）。
//
// 只探回环地址：状态文件里的地址本就该是通途自己的回环监听；出现别的形态
// （手改、损坏）一律按「不活着」处理，不对外网地址发起任何连接。
func addrListening(addr string) bool {
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
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
