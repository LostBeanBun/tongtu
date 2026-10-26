package app

// health.go 实现「假活自愈」与睡眠/换网恢复。
//
// 假活：进程还在、界面还显示「运行中」，但对外已经不能正常工作——本地监听没了、
// hosts 托管块被别的程序改掉了、所有 DoH 端点都失败了。这里用一个小状态机定期检查，
// 发现**可自愈**的问题就当场修：重绑本地监听、重写 hosts、重置 DNS 缓存并重新预热。
//
// 不抢不覆盖：系统代理如果被别的程序接管，只标记为降级并说明原因，绝不再改注册表。
//
// 睡眠/换网：窗口收到 WM_POWERBROADCAST 恢复事件时立即触发；跨平台侧另有网络指纹轮询
// （接口名、状态、MTU、地址的哈希），变化时触发同一套恢复。恢复动作串行化并防抖，
// 同一次睡眠/换网引发的多个事件只会真正执行一遍。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"tongtu/internal/hosts"
	"tongtu/internal/resolver"
	"tongtu/internal/sysproxy"
)

// Health 是运行健康度快照（供 status / 诊断 / 窗口状态条使用）。
type Health struct {
	State        string    `json:"state"` // ok / degraded / healing / paused / stopped
	Reason       string    `json:"reason,omitempty"`
	CheckedAt    time.Time `json:"checked_at,omitempty"`
	LastHeal     time.Time `json:"last_heal,omitempty"`
	HealCount    int64     `json:"heal_count"`
	RecoverCount int64     `json:"recover_count"`
	LastRecover  time.Time `json:"last_recover,omitempty"`
}

// 健康度状态词。放在 app 包里，CLI / 窗口 / 诊断共用同一套判断。
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
	HealthHealing  = "healing"
	HealthPaused   = "paused"
	HealthStopped  = "stopped"
)

const (
	// healthCheckEvery 是主动健康检查周期。只查本地监听与缓存状态，不访问外网。
	healthCheckEvery = 15 * time.Second
	// networkPollEvery 是网络指纹轮询周期：睡眠恢复有窗口消息，换网则靠它兜底。
	networkPollEvery = 5 * time.Second
	// recoverDebounce 把同一次睡眠/换网引发的多个事件合并成一次恢复。
	recoverDebounce = 3 * time.Second
	// dnsResetCooldown 避免在长时间断网时每 15 秒就重置一次 DNS。
	dnsResetCooldown = 2 * time.Minute
	// healCooldown 限制同一种自愈动作的重试频率：:443 被别的程序长期占用、
	// 或 hosts 因权限问题写不进去时，不该每 15 秒重试并刷一遍日志。
	healCooldown = time.Minute
	// healthHostsTimeout 是健康自愈重写 hosts 的上限：比常规 2 分钟短，避免界面长时间等锁。
	healthHostsTimeout = 20 * time.Second
)

// 健康问题类型。
const (
	issueProxy       = "proxy"
	issueHosts       = "hosts"
	issueLocalMITM   = "local_mitm"
	issueDNS         = "dns"
	issueSystemProxy = "system_proxy"
)

type healthIssue struct {
	kind     string
	reason   string
	healable bool
}

// healthLoop 定期检查健康度并轮询网络指纹，直到 ctx 取消。
func (a *App) healthLoop(ctx context.Context) {
	a.netMu.Lock()
	a.netFingerprint = networkFingerprint()
	a.netMu.Unlock()

	a.CheckHealth()

	netTick := time.NewTicker(networkPollEvery)
	healthTick := time.NewTicker(healthCheckEvery)
	defer netTick.Stop()
	defer healthTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-netTick.C:
			a.checkNetworkChange()
		case <-healthTick.C:
			a.CheckHealth()
		}
	}
}

// checkNetworkChange 比较网络指纹；发生变化时触发一次恢复。
//
// 指纹为空表示当前拿不到可用接口（例如刚睡醒、网络栈还没起来）：先只更新基线；
// 等网络恢复、指纹从空变回非空时再触发一次恢复。启动时已经拿到网络则只记基线。
func (a *App) checkNetworkChange() {
	fp := networkFingerprint()
	a.netMu.Lock()
	prev := a.netFingerprint
	a.netFingerprint = fp
	a.netMu.Unlock()
	if !shouldRecover(prev, fp) {
		return
	}
	a.log.Info("检测到网络接口变化，开始恢复", "reason", "网络接口变化")
	go func() {
		if err := a.RecoverNetwork("网络接口变化"); err != nil {
			a.log.Warn("网络变化恢复未完全成功", "err", err)
		}
	}()
}

// shouldRecover 判断一次指纹变化是否值得触发恢复：当前必须有可用网络（非空），
// 且与基线不同。断网（非空 → 空）只更新基线，等网络回来时再恢复一次。
func shouldRecover(prev, next string) bool {
	return next != "" && next != prev
}

// CheckHealth 执行一次健康检查，并在发现可自愈问题时尝试修复。
// 返回检查后的快照；并发安全。
func (a *App) CheckHealth() Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.checkHealthLocked()
}

func (a *App) checkHealthLocked() Health {
	now := time.Now()
	a.health.CheckedAt = now
	switch {
	case a.ln == nil || a.stopped:
		a.health.State = HealthStopped
		a.health.Reason = ""
		return a.health
	case a.paused.active():
		a.health.State = HealthPaused
		a.health.Reason = ""
		return a.health
	}

	issues := a.healthIssuesLocked()
	healed := make([]string, 0, len(issues))
	remain := make([]healthIssue, 0, len(issues))
	for _, iss := range issues {
		if !iss.healable {
			remain = append(remain, iss)
			continue
		}
		if err := a.healIssueLocked(iss); err != nil {
			iss.reason = iss.reason + "（自愈失败: " + err.Error() + "）"
			remain = append(remain, iss)
			continue
		}
		healed = append(healed, iss.kind)
	}
	if len(healed) > 0 {
		a.health.LastHeal = now
		a.health.HealCount += int64(len(healed))
		a.log.Info("健康自愈完成", "actions", healed)
	}

	if len(remain) > 0 {
		reasons := make([]string, 0, len(remain))
		for _, iss := range remain {
			reasons = append(reasons, iss.reason)
		}
		a.health.State = HealthDegraded
		a.health.Reason = strings.Join(reasons, "；")
		return a.health
	}
	a.health.State = HealthOK
	a.health.Reason = ""
	return a.health
}

// healthIssuesLocked 检查当前是否存在「看起来在跑、实际不工作」的问题（需持有 a.mu）。
func (a *App) healthIssuesLocked() []healthIssue {
	var out []healthIssue

	if !addrAlive(a.proxyAddr()) {
		out = append(out, healthIssue{kind: issueProxy, reason: "本地代理监听无响应", healable: true})
	}

	dnsFailed := dnsAllFailed(a.res.Health())
	if dnsFailed && time.Since(a.lastDNSReset) >= dnsResetCooldown {
		out = append(out, healthIssue{kind: issueDNS, reason: "所有 DoH 端点均失败", healable: true})
	}

	if a.hostsOn {
		if !hosts.Managed(hosts.DefaultPath()) {
			// 网络全断时先不重写 hosts：解析会等满超时，把状态查询一起拖住。
			// 刚失败过也先等一个冷却周期，避免非管理员场景每 15 秒重试一次。
			out = append(out, healthIssue{
				kind:     issueHosts,
				reason:   "hosts 托管块已丢失",
				healable: !dnsFailed && time.Since(a.lastHostsHeal) >= healCooldown,
			})
		}
		if a.mitmCa != nil && (!a.px.Local443Up() || !addrAlive("127.0.0.1:443")) {
			// :443 可能被别的程序长期占用：失败后同样等冷却，别每 15 秒刷一条警告。
			out = append(out, healthIssue{
				kind:     issueLocalMITM,
				reason:   "本地 :443 监听已失效",
				healable: time.Since(a.lastLocalRebind) >= healCooldown,
			})
		}
	}

	if a.sysOn && !a.sysProxyHeld() {
		// 被别的程序接管：只降级，不抢回来（既有承诺）。
		out = append(out, healthIssue{kind: issueSystemProxy, reason: "系统代理已被其他程序接管", healable: false})
	}
	return out
}

// healIssueLocked 修复一个可自愈问题（需持有 a.mu）。
func (a *App) healIssueLocked(iss healthIssue) error {
	switch iss.kind {
	case issueProxy:
		return a.restartProxyLocked()
	case issueHosts:
		a.lastHostsHeal = time.Now()
		if !a.applyHostsLockedTimeout("健康自愈：hosts 托管块丢失", healthHostsTimeout) {
			return fmt.Errorf("hosts 更新失败（可能需要管理员权限）")
		}
		return nil
	case issueLocalMITM:
		return a.rebindLocalMITMLocked()
	case issueDNS:
		a.lastDNSReset = time.Now()
		a.res.Reset()
		a.px.ResetNetwork()
		if a.warm && a.loopCtx != nil {
			warmHosts := a.warmHosts()
			go a.px.Warm(a.loopCtx, warmHosts, 4)
		}
		a.log.Info("已重置 DNS 缓存与端点顺序，准备重新解析")
		return nil
	}
	return nil
}

// restartProxyLocked 重新绑定代理监听（需持有 a.mu）。
//
// 正常情况下不会走到这里；只有 Serve 异常退出、监听被系统关闭时健康检查才会触发。
// 端口被占用时退回空闲回环端口，并让系统代理跟着换口（只在本实例仍持有时）。
func (a *App) restartProxyLocked() error {
	if a.stopped {
		return nil
	}
	oldAddr := a.proxyAddr()
	if a.srv != nil {
		_ = a.srv.Close()
	}
	ln, err := net.Listen("tcp", oldAddr)
	if err != nil {
		fallback, ferr := net.Listen("tcp", freeLoopbackAddr(a.cfg.Listen))
		if ferr != nil {
			return fmt.Errorf("重新监听 %s 失败: %w", oldAddr, err)
		}
		ln = fallback
	}
	srv := &http.Server{Handler: a.px, ReadHeaderTimeout: 15 * time.Second}
	a.ln = ln
	a.srv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("代理服务异常退出", "err", err)
		}
	}()

	newAddr := ln.Addr().String()
	if newAddr != oldAddr {
		a.log.Warn("代理监听已重新绑定", "旧", oldAddr, "新", newAddr)
	}
	if a.sysOn {
		changed, rerr := sysproxy.Retarget(newAddr)
		switch {
		case rerr != nil:
			a.log.Warn("系统代理换口失败（注册表可能仍指向旧端口）", "err", rerr)
		case changed:
			a.log.Info("系统代理已跟随代理端口更新", "addr", newAddr)
		}
	}
	return nil
}

// rebindLocalMITMLocked 重新绑定 hosts 直连用的本地 :443/:80（需持有 a.mu）。
func (a *App) rebindLocalMITMLocked() error {
	if a.mitmCa == nil || a.stopped {
		// 已进入退出流程：不要在 Shutdown 关掉监听之后又把它打开。
		return nil
	}
	// 记下本次尝试：失败时健康检查会等一个冷却周期再试（见 healCooldown）。
	a.lastLocalRebind = time.Now()
	a.px.CloseLocal()
	a.mitmLocal443, a.mitmLocal80 = false, false
	a.startLocalMITM()
	if !a.mitmLocal443 {
		return fmt.Errorf("本地 :443 重新绑定失败（端口可能被占用）")
	}
	if a.hostsOn && !a.applyHostsLockedTimeout("健康自愈：本地 :443 重新绑定", healthHostsTimeout) {
		return fmt.Errorf("hosts 未更新")
	}
	a.log.Info("本地 :443 监听已重新绑定")
	return nil
}

// RecoverNetwork 在睡眠恢复 / 换网后重建与网络绑定的状态：DNS 缓存、优选 IP、空闲连接、
// hosts 解析与本地监听。串行执行并防抖，同一次事件重复触发只会真正跑一遍。
//
// 不抢系统代理：只在本实例仍持有时确保它指向当前端口；被别的程序接管则只记日志。
func (a *App) RecoverNetwork(reason string) error {
	a.recoverMu.Lock()
	defer a.recoverMu.Unlock()

	a.mu.Lock()
	if a.ln == nil || a.stopped {
		a.mu.Unlock()
		return nil
	}
	if time.Since(a.lastRecover) < recoverDebounce {
		a.mu.Unlock()
		return nil
	}
	now := time.Now()
	a.lastRecover = now
	a.health.State = HealthHealing
	a.health.Reason = reason
	a.health.RecoverCount++
	a.health.LastRecover = now

	hostsOn := a.hostsOn
	needLocal := hostsOn && a.mitmCa != nil &&
		(!a.px.Local443Up() || !addrAlive("127.0.0.1:443"))
	warm := a.warm
	loopCtx := a.loopCtx
	warmHosts := a.warmHosts()
	a.mu.Unlock()

	// 换网后旧缓存与旧连接都不可信：先整体作废，后续请求重新解析、重新竞速。
	a.res.Reset()
	a.px.ResetNetwork()

	var errs []error
	if needLocal {
		a.mu.Lock()
		err := a.rebindLocalMITMLocked()
		a.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}

	if hostsOn {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		if _, err := a.hm.Apply(ctx); err != nil {
			errs = append(errs, fmt.Errorf("刷新 hosts: %w", err))
		}
		cancel()
		// 恢复期间用户可能按了暂停：别把 hosts 又写回去。
		a.mu.Lock()
		stillOn := a.hostsOn
		a.mu.Unlock()
		if !stillOn {
			_, _ = a.hm.Remove()
		}
	}

	// 系统代理：仍由我们持有时确保指向当前端口；被别的程序接管则不抢。
	a.mu.Lock()
	if a.sysOn {
		addr := a.proxyAddr()
		held, err := sysproxy.Held(addr)
		switch {
		case err != nil:
			a.log.Debug("网络恢复：读取系统代理状态失败", "err", err)
		case !held:
			a.log.Warn("网络恢复：系统代理已被其他程序接管，保持不覆盖")
		default:
			if changed, rerr := sysproxy.Retarget(addr); rerr != nil {
				errs = append(errs, fmt.Errorf("更新系统代理地址: %w", rerr))
			} else if changed {
				a.log.Info("网络恢复：系统代理地址已更新", "addr", addr)
			}
		}
	}
	a.lastRecover = time.Now()
	a.mu.Unlock()

	if warm && loopCtx != nil {
		go a.px.Warm(loopCtx, warmHosts, 4)
	}
	a.log.Info("网络恢复完成", "reason", reason, "hosts", hostsOn)
	a.CheckHealth()
	return errors.Join(errs...)
}

// dnsAllFailed 报告是否所有 DoH 端点都处于失败状态。
// 从未使用过的端点（没有 LastError）不算失败，避免刚启动就误判。
func dnsAllFailed(hs []resolver.Health) bool {
	if len(hs) == 0 {
		return false
	}
	for _, h := range hs {
		if h.LastError == "" {
			return false
		}
	}
	return true
}

// ifaceSnapshot 是网络指纹用到的单个接口快照。
type ifaceSnapshot struct {
	Name  string
	Flags net.Flags
	MTU   int
	Addrs []string
}

// networkFingerprint 返回当前「活动网络」的指纹：接口名、状态、MTU 与非回环地址。
// 换 Wi-Fi、插网线、VPN 开关、DHCP 换地址都会改变它；纯时间流逝不会。
func networkFingerprint() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	snaps := make([]ifaceSnapshot, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		ips := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				ips = append(ips, addr.String())
				continue
			}
			if ip.IsLoopback() {
				continue
			}
			ips = append(ips, ip.String())
		}
		if len(ips) == 0 {
			continue
		}
		sort.Strings(ips)
		snaps = append(snaps, ifaceSnapshot{Name: iface.Name, Flags: iface.Flags, MTU: iface.MTU, Addrs: ips})
	}
	if len(snaps) == 0 {
		// 没有任何「已启用且有非回环地址」的接口：用空串表示「当前无网」，
		// 这样断网时只更新基线，等网络回来（空 → 非空）再触发一次恢复。
		return ""
	}
	return fingerprintSnapshots(snaps)
}

// fingerprintSnapshots 把接口快照压成稳定指纹（纯函数，便于单测）。
func fingerprintSnapshots(snaps []ifaceSnapshot) string {
	out := append([]ifaceSnapshot(nil), snaps...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	var b strings.Builder
	for _, s := range out {
		fmt.Fprintf(&b, "%s|%d|%d|%s\n", s.Name, s.Flags, s.MTU, strings.Join(s.Addrs, ","))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
