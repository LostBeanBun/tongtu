// Package app 编排 tongtu 的核心运行时：本地代理服务 + 系统代理/hosts 双模式。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"tongtu/internal/config"
	"tongtu/internal/hosts"
	"tongtu/internal/mitm"
	"tongtu/internal/proxy"
	"tongtu/internal/resolver"
	"tongtu/internal/rules"
	"tongtu/internal/sysproxy"
)

// Status 是状态查询的返回体。
type Status struct {
	Running     bool   `json:"running"`
	Listen      string `json:"listen"`
	UptimeSec   int64  `json:"uptime_sec"`
	SystemProxy bool   `json:"system_proxy"`         // 系统代理「实际生效中」（仍由本实例持有）
	ProxyNote   string `json:"proxy_note,omitempty"` // 系统代理没生效的原因（如被别的程序接管）
	Hosts       bool   `json:"hosts"`
	MITM        bool   `json:"mitm"`                // 无 SNI 反代是否启用
	MITMNote    string `json:"mitm_note,omitempty"` // 反代未启用的原因（启用时为空）
	MITMLocal   bool   `json:"mitm_local"`          // hosts 直连用本地 :443 是否就绪
	Paused      bool   `json:"paused"`              // 是否处于「一键暂停」（进程仍在，对外影响已撤回）
	// PauseUntil 是定时暂停的到期时间（Unix 秒；0 = 未定时，只能手动恢复）。
	// PauseRemainSec 是距自动恢复的秒数（未定时或已过期时为 0）。
	// 两个字段都只在 Paused 为真时有意义：界面据此显示「还剩几分钟」。
	PauseUntil     int64             `json:"pause_until,omitempty"`
	PauseRemainSec int64             `json:"pause_remain_sec,omitempty"`
	Services       []string          `json:"services"`
	Proxy          proxy.Snapshot    `json:"proxy"`
	DNS            []resolver.Health `json:"dns"`
	Health         Health            `json:"health"`
}

// Options 由调用方注入 App 所需依赖。
type Options struct {
	Config     *config.Config
	Rules      *rules.Set
	SysProxy   bool     // 是否接管系统代理
	HostsMode  bool     // 是否接管 hosts
	MITMCA     *mitm.CA // 无 SNI 反代根证书；nil 表示不启用
	MITMNote   string   // 反代未能启用的原因（启用时留空；用于给用户交代，不参与逻辑）
	Warmup     bool     // 启动后在后台预热反代/白名单域名（见 proxy.Warm）
	ConfigPath string   // 配置文件路径（托盘切换后持久化用；空则用默认路径）
	Log        *slog.Logger
}

// App 持有全部运行时组件及其生命周期。
type App struct {
	cfg     *config.Config
	cfgPath string
	log     *slog.Logger
	res     *resolver.Resolver
	px      *proxy.Proxy
	hm      *hosts.Manager
	rs      *rules.Set
	svcIDs  []string

	mitmCa         *mitm.CA
	mitmNote       string   // 反代未启用的原因（启用成功时清空）
	mitmLocalNames []string // 命中反代名单、需写 127.0.0.1 的 hosts 主机
	mitmLocal443   bool
	mitmLocal80    bool

	warm bool // 启动后是否预热反代/白名单域名

	// paused 记录被「一键暂停」撤掉的项（nil = 当前没有暂停）。
	// 与 Shutdown 的区别：暂停只撤掉对外影响，进程与本地监听继续运行，随时可以恢复。
	paused *pauseState
	// pauseUntil 是定时暂停的到期时间（零值 = 未定时）；pauseGen 是定时器代号：
	// 每次暂停 / 恢复都抬一，挂着的旧定时器醒来发现代号对不上就直接退出，
	// 这样「暂停 30 分钟」之后又手动恢复、再暂停 10 分钟，不会被前一个定时器误恢复。
	pauseUntil time.Time
	pauseGen   int64

	mu           sync.Mutex
	ln           net.Listener
	srv          *http.Server
	sysSaved     *sysproxy.Values
	sysOn        bool
	hostsOn      bool
	wantSysProxy bool
	wantHosts    bool
	startedAt    time.Time
	stopped      bool
	cancel       context.CancelFunc
	loopCtx      context.Context

	// 健康自愈与网络恢复（见 health.go）。health 受 mu 保护；
	// recoverMu 串行化恢复动作，netFingerprint 受 netMu 保护。
	health          Health
	recoverMu       sync.Mutex
	lastRecover     time.Time
	lastDNSReset    time.Time
	lastHostsHeal   time.Time
	lastLocalRebind time.Time
	netMu           sync.Mutex
	netFingerprint  string
}

// New 创建 App（不开始监听）。
func New(opts Options) *App {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	res := resolver.New(resolver.Config{
		Endpoints:   opts.Config.DNS.Endpoints,
		Timeout:     opts.Config.DNSTimeout(),
		CacheMin:    time.Duration(opts.Config.DNS.CacheMinSec) * time.Second,
		CacheMax:    time.Duration(opts.Config.DNS.CacheMaxSec) * time.Second,
		RemoteHosts: opts.Config.DNS.RemoteHosts,
		StaticHosts: opts.Config.DNS.StaticHosts,
	}, log)
	// 自定义站点（config.custom）与内置服务组合并成同一个白名单：
	// 后续的代理分流、hosts 覆盖、反代名单都只看这一个 Set。
	if opts.Rules != nil {
		opts.Rules.AddPatterns(opts.Config.ProxyPatterns())
	}
	px := proxy.New(opts.Rules, res, log)
	px.SetLookupAll(res.LookupAll)                 // 隧道路径也用多端点并查候选
	px.SetLookupAllUncached(res.LookupAllUncached) // 无 SNI 优选用新鲜候选
	hm := hosts.New(hosts.DefaultPath(), opts.Rules.Hosts(), res, opts.Config.HostsRefresh(), log)

	a := &App{
		cfg:          opts.Config,
		cfgPath:      opts.ConfigPath,
		log:          log,
		res:          res,
		px:           px,
		hm:           hm,
		rs:           opts.Rules,
		svcIDs:       append([]string(nil), opts.Config.Services...),
		wantSysProxy: opts.SysProxy,
		wantHosts:    opts.HostsMode,
		mitmNote:     opts.MITMNote,
		warm:         opts.Warmup,
		health:       Health{State: HealthStopped},
	}
	// 无 SNI 反代：SNI 封锁域名本地解密、出站剥离 SNI（候选 IP 来自干净 DoH）
	if opts.MITMCA != nil {
		a.configureMITM(opts.MITMCA)
	}
	return a
}

// configureMITM 装配无 SNI 反代（初始化阶段或持有 a.mu 时调用）。
//
// 「哪些域名要反代」只看配置里的 mitm.domains（含内置默认值，见 config.Default）：
// 反代是有代价的——本地解密 + 出站剥离 SNI——只该用在实测被 SNI 封锁的域名上。
// 尤其不能图省事写 *.steampowered.com：login.steampowered.com 这类站点必须带 SNI，
// 剥离后握手直接失败（实测 tls: internal error），会把本来正常的页面一起弄坏。
func (a *App) configureMITM(ca *mitm.CA) {
	a.px.EnableMITM(ca, func(host string) bool {
		return rules.MatchPatterns(host, a.cfg.MITMPatterns())
	})
	a.mitmCa = ca
	a.mitmNote = "" // 已启用，「未启用原因」不再适用
	a.mitmLocalNames = a.mitmLocalNamesFor(a.rs)
}

// warmHosts 返回启动预热的候选主机名：hosts 覆盖名单 + 非通配的反代域名。
// 通配模式（如 *.steamcommunity.com）无法枚举，靠 hosts 名单里的具体主机覆盖。
func (a *App) warmHosts() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	if a.rs != nil {
		for _, h := range a.rs.Hosts() {
			add(h)
		}
	}
	if a.cfg != nil {
		for _, d := range a.cfg.MITMPatterns() {
			if !strings.Contains(d, "*") {
				add(d)
			}
		}
	}
	return out
}

// mitmLocalNamesFor 返回「命中反代名单」的 hosts 主机名（这些需写 127.0.0.1）。
func (a *App) mitmLocalNamesFor(rs *rules.Set) []string {
	if rs == nil {
		return nil
	}
	var out []string
	for _, h := range rs.Hosts() {
		if rules.MatchPatterns(h, a.cfg.MITMPatterns()) {
			out = append(out, h)
		}
	}
	return out
}

// Start 启动本地代理监听，并按需接管系统代理与 hosts。可重复调用会报错。
func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln != nil {
		return fmt.Errorf("服务已在运行")
	}

	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		// 端口被占用不该让整个服务起不来：换一个空闲回环端口继续。
		// 系统代理指向的是实际监听地址（见 proxyAddr），因此对用户是透明的。
		fallback, ferr := net.Listen("tcp", freeLoopbackAddr(a.cfg.Listen))
		if ferr != nil {
			return fmt.Errorf("监听 %s 失败: %w", a.cfg.Listen, err)
		}
		a.log.Warn("代理端口被占用，已改用空闲端口",
			"配置", a.cfg.Listen, "实际", fallback.Addr().String())
		ln = fallback
	}
	a.ln = ln
	a.srv = &http.Server{Handler: a.px, ReadHeaderTimeout: 15 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.loopCtx = ctx
	go func() {
		if err := a.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("代理服务异常退出", "err", err)
		}
	}()
	a.startedAt = time.Now()

	// 接管系统代理（失败不致命，hosts 与代理本身仍可用）
	if a.wantSysProxy {
		if saved, err := sysproxy.Enable(a.proxyAddr()); err != nil {
			a.log.Error("接管系统代理失败", "err", err)
		} else {
			a.sysSaved = saved
			a.sysOn = true
		}
	}

	// 接管 hosts（需要管理员权限）
	if a.wantHosts {
		a.startLocalMITM()
		if _, err := a.hm.Apply(ctx); err != nil {
			a.log.Error("hosts 模式启动失败（系统代理模式不受影响）", "err", err)
		} else {
			a.hostsOn = true
			go a.hm.Run(ctx)
		}
	}

	// 后台预热：把反代域名的「候选竞速」成本从第一次真实访问提前到启动时。
	// 预热写入的优选/解析缓存与真实请求共用，因此第一个请求能直接走快路径。
	// ctx 随 Shutdown 取消，退出时预热自动中止。
	if a.warm {
		go a.px.Warm(ctx, a.warmHosts(), 4)
	}

	// 健康检查与网络指纹轮询：假活自愈、睡眠/换网恢复都从这里开始。
	a.health = Health{State: HealthOK, CheckedAt: time.Now()}
	go a.healthLoop(ctx)
	return nil
}

// startLocalMITM 启动 hosts 直连用的本地 MITM 监听（需持有 a.mu）。
// :443 绑定成功后，反代域名在 hosts 中写 127.0.0.1；失败则降级为真实 IP。
// [::1] 也尽力绑一份：域名自带的 AAAA 会让 IPv6 优先的客户端绕过本地捕捉。
func (a *App) startLocalMITM() {
	if a.mitmCa == nil || a.mitmLocal443 {
		return
	}
	if err := a.px.ListenLocal443(); err != nil {
		a.log.Warn("本地 MITM :443 监听失败：hosts 模式下反代域名仅在系统代理路径可用", "err", err)
		return
	}
	a.mitmLocal443 = true
	a.hm.SetLocalHosts(a.mitmLocalNames)
	// 只有在 [::1]:443 确实绑上时才让 hosts 写 ::1（否则会让客户端先撞一次拒连）
	a.hm.SetLocalIPv6(a.px.LocalIPv6Up())
	if err := a.px.ListenLocal80(); err != nil {
		a.log.Debug("本地 :80 监听失败（HTTP 直连转发不可用，不影响 HTTPS）", "err", err)
	} else {
		a.mitmLocal80 = true
	}
	a.log.Info("hosts 直连 MITM 已就绪",
		"domains", len(a.mitmLocalNames), "ipv6", a.px.LocalIPv6Up())
}

// stopLocalMITM 关闭本地直连监听（需持有 a.mu）。
func (a *App) stopLocalMITM() {
	if !a.mitmLocal443 && !a.mitmLocal80 {
		return
	}
	a.px.CloseLocal()
	a.mitmLocal443, a.mitmLocal80 = false, false
	a.hm.SetLocalHosts(nil) // 还原为真实 IP 解析
	a.hm.SetLocalIPv6(false)
}

// Shutdown 优雅停止：还原 hosts、还原系统代理、关闭监听。幂等。
func (a *App) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	cancel := a.cancel
	srv := a.srv
	sysOn, saved, hostsOn := a.sysOn, a.sysSaved, a.hostsOn
	a.sysOn, a.hostsOn, a.sysSaved = false, false, nil
	a.mitmLocal443, a.mitmLocal80 = false, false
	a.health.State = HealthStopped
	a.health.Reason = ""
	a.health.CheckedAt = time.Now()
	// 退出是终态：把挂着的「定时暂停」一并作废，别让它在进程退出后还试图恢复。
	a.pauseGen++
	a.pauseUntil = time.Time{}
	a.paused = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel() // 停止 hosts 刷新循环
	}
	var errs []error
	if hostsOn {
		if _, err := a.hm.Remove(); err != nil {
			errs = append(errs, fmt.Errorf("移除 hosts 托管块: %w", err))
		}
	}
	a.px.CloseLocal() // 本地 MITM 直连监听（幂等）
	if sysOn {
		out, err := sysproxy.Disable(saved)
		switch {
		case err != nil:
			errs = append(errs, err)
		case out == sysproxy.NotHeld:
			a.log.Warn("系统代理已被其他程序接管，退出时未还原（避免覆盖对方的设置）")
		}
	}
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	a.log.Info("服务已停止，系统状态已还原")
	return errors.Join(errs...)
}

// SetMode 运行时切换系统代理 / hosts 两个开关（供托盘菜单与控制 API 使用）。
//
// 系统代理一栏比较的是「实际是否仍由本实例持有」而不是内部标志：被别的程序接管后，
// 界面显示为关闭、点一下就能重新夺回（重新快照当前值作为新的原值），不会出现
// 「显示已开、点了却什么都不发生」。
func (a *App) SetMode(sys, hostsWant bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil {
		return fmt.Errorf("服务未启动")
	}
	var errs []error

	held := a.sysProxyHeld()
	sysChanged := sys != held
	hostsChanged := hostsWant != a.hostsOn

	switch {
	case sys && !held:
		saved, err := sysproxy.Enable(a.proxyAddr())
		if err != nil {
			errs = append(errs, err)
		} else {
			a.sysSaved = saved
			a.sysOn = true
		}
	case !sys && held:
		// 我们确实还持有，用户要关：Disable 负责还原到接管前的值。
		out, err := sysproxy.Disable(a.sysSaved)
		switch {
		case err != nil:
			errs = append(errs, err)
		case out == sysproxy.NotHeld:
			a.log.Warn("系统代理刚刚被其他程序接管，未还原（避免覆盖对方的设置）")
		}
		a.sysOn = false
		a.sysSaved = nil
	}

	if hostsChanged {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if hostsWant {
			a.startLocalMITM()
			if _, err := a.hm.Apply(ctx); err != nil {
				errs = append(errs, err)
			} else {
				a.hostsOn = true
				go a.hm.Run(a.loopCtx)
			}
		} else {
			if _, err := a.hm.Remove(); err != nil {
				errs = append(errs, err)
			} else {
				a.hostsOn = false
				a.stopLocalMITM()
			}
		}
	}
	// 把本次真正改变的开关写回配置，使托盘的操作在下次启动时仍然有效。
	// 记录的是用户的**意图**：即使本次因权限等原因未生效（如非管理员下的 hosts），
	// 下次以管理员启动时仍会按配置尝试接管。
	// 只写变动的项，避免一次性启动标志（如 --no-sysproxy）被误固化进配置。
	if sysChanged {
		a.cfg.SystemProxy = sys
	}
	if hostsChanged {
		a.cfg.Hosts = hostsWant
	}
	if sysChanged || hostsChanged {
		a.saveConfigLocked()
	}
	// 手动打开某一项后，它就不再属于「被暂停」的项了（暂停按钮上的数字/状态要跟着变）。
	a.unpauseLocked(sys, hostsWant, false)
	return errors.Join(errs...)
}

// unpauseLocked 把用户手动打开（或已恢复）的项从「暂停」记录里去掉（需持有 a.mu）。
func (a *App) unpauseLocked(sys, hosts, mitm bool) {
	if a.paused == nil {
		return
	}
	if sys {
		a.paused.sys = false
	}
	if hosts {
		a.paused.hosts = false
	}
	if mitm {
		a.paused.mitm = false
	}
	if !a.paused.active() {
		a.paused = nil
		// 暂停项被用户逐个手动打开到「没有暂停项」时，定时器也该一起作废：
		// 否则它到点会再「恢复」一次，把用户随后手动关掉的开关重新打开。
		a.pauseGen++
		a.pauseUntil = time.Time{}
	}
}

// SetMITM 运行时开关「无 SNI 反代」（托盘菜单用）。
// 启用前要求根证书已安装；hosts 模式在用时同步改写 hosts 指向。
func (a *App) SetMITM(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil {
		return fmt.Errorf("服务未启动")
	}
	if on == (a.mitmCa != nil) {
		a.cfg.MITM.Enabled = on
		a.saveConfigLocked()
		return nil
	}
	if on {
		ca, err := mitm.LoadOrCreate(config.Dir())
		if err != nil {
			return fmt.Errorf("加载本地 CA 失败: %w", err)
		}
		if installed, _ := mitm.Installed(ca); !installed {
			return fmt.Errorf("根证书尚未安装，请先执行 tongtu cert install")
		}
		a.configureMITM(ca)
		a.cfg.MITM.Enabled = true
		if a.hostsOn {
			a.startLocalMITM()
			a.applyHostsLocked("启用无 SNI 反代")
		}
		a.unpauseLocked(false, false, true)
		a.log.Info("无 SNI 反代已启用")
	} else {
		if err := a.disableMITMLocked(); err != nil {
			return err
		}
		a.cfg.MITM.Enabled = false
		a.log.Info("无 SNI 反代已停用")
	}
	a.saveConfigLocked()
	return nil
}

// SetServices 运行时切换加速服务组（托盘菜单用）。至少需保留一个。
func (a *App) SetServices(ids []string) error {
	rs, err := rules.Load(ids)
	if err != nil {
		return err
	}
	rs.AddPatterns(a.cfg.ProxyPatterns()) // 自定义站点不随服务组切换丢失
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil {
		return fmt.Errorf("服务未启动")
	}
	a.applyRulesLocked(rs, ids, a.cfg.Custom, a.cfg.MITM.Domains)
	a.saveConfigLocked()
	a.log.Info("加速服务已切换", "services", a.svcIDs)
	return nil
}

// applyRulesLocked 应用一份新的匹配集合与相应的配置（需持有 a.mu）。
//
// 代理分流、hosts 覆盖、反代名单三者必须同时更新：只改其中一个会留下
// 「白名单里有、hosts 里没有」这种半生效状态，排查起来非常费劲。
func (a *App) applyRulesLocked(rs *rules.Set, svcIDs []string, custom config.CustomConfig, mitmDomains []string) {
	a.rs = rs
	a.svcIDs = append([]string(nil), svcIDs...)
	a.cfg.Services = append([]string(nil), svcIDs...)
	a.cfg.Custom = config.CustomConfig{
		Domains:     append([]string(nil), custom.Domains...),
		MITMDomains: append([]string(nil), custom.MITMDomains...),
	}
	a.cfg.MITM.Domains = append([]string(nil), mitmDomains...)
	a.px.SetRules(rs)
	a.hm.SetHosts(rs.Hosts())

	// 反代名单可能变了：重新装配 match（EnableMITM 支持整体替换，见 proxy.EnableMITM）。
	if a.mitmCa != nil {
		a.configureMITM(a.mitmCa)
		a.hm.SetLocalHosts(a.mitmLocalNames)
		a.hm.SetLocalIPv6(a.mitmLocal443 && a.px.LocalIPv6Up())
	}
	if a.hostsOn {
		a.applyHostsLocked("更新加速规则")
	}
}

// ReloadRules 重新应用配置文件里的「服务组 / 自定义站点 / 反代域名」，返回是否真的发生了变化。
//
// 只处理这几个字段：改端口、DNS 端点、日志级别都需要重建组件或重启进程，假装热更新只会
// 让人以为生效了。界面上的「配置已重新加载」按 changed 决定要不要提示。
func (a *App) ReloadRules(next *config.Config) (bool, error) {
	if next == nil {
		return false, fmt.Errorf("配置为空")
	}
	rs, err := rules.Load(next.Services)
	if err != nil {
		return false, err
	}
	rs.AddPatterns(next.ProxyPatterns())

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil || a.stopped {
		return false, fmt.Errorf("服务未启动")
	}
	if equalStrings(a.svcIDs, next.Services) &&
		equalStrings(a.cfg.ProxyPatterns(), next.ProxyPatterns()) &&
		equalStrings(a.cfg.MITMPatterns(), next.MITMPatterns()) {
		return false, nil
	}
	a.applyRulesLocked(rs, next.Services, next.Custom, next.MITM.Domains)
	// 刻意不写回配置文件：文件本身就是用户刚保存的那份，再写一遍会丢注释、也会把
	// 我们没热更新的字段（端口 / 日志级别）用旧值覆盖回去。
	// 也不在这里打日志：调用方（watchConfigFile）会用「配置已热生效」把变化说清楚，
	// 一件事只留一条日志。
	return true, nil
}

// PauseState 记载「一键暂停」撤掉了哪几项。
type pauseState struct {
	sys   bool
	hosts bool
	mitm  bool
}

func (p *pauseState) active() bool { return p != nil && (p.sys || p.hosts || p.mitm) }

// StopAll 一键暂停：还原系统代理、退出 hosts 接管、关掉无 SNI 反代，但进程与本地监听继续运行。
//
// 与 Shutdown 的区别：Shutdown 是终态（退出，不再恢复）；StopAll 只撤掉「对外的影响」，
// 配置里的意图不动，随时可以用 Resume 原样回来。这两件事混在一起做，用户就分不清
// 「我想暂时关掉」和「我要退出」了。
func (a *App) StopAll() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopAllLocked()
}

// stopAllLocked 是 StopAll 的实现（需持有 a.mu）。
//
// 任何一次暂停都先作废旧定时器：暂停不指定时长时，用户要的是「一直停到我点恢复」，
// 不能让上一轮遗留的 30 分钟定时器在半路把它打开。
func (a *App) stopAllLocked() error {
	if a.ln == nil || a.stopped {
		return fmt.Errorf("服务未启动")
	}
	a.pauseGen++
	a.pauseUntil = time.Time{}
	p := &pauseState{
		sys:   a.sysOn && a.sysProxyHeld(),
		hosts: a.hostsOn,
		mitm:  a.mitmCa != nil,
	}
	if !p.active() {
		a.paused = nil
		return nil
	}
	var errs []error
	// 顺序与 Shutdown 一致：先撤反代（它依赖 hosts 指向本地），再退 hosts，最后还原系统代理。
	if p.mitm {
		if err := a.disableMITMLocked(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.hosts && a.hostsOn {
		if _, err := a.hm.Remove(); err != nil {
			errs = append(errs, fmt.Errorf("退出 hosts 接管: %w", err))
		} else {
			a.hostsOn = false
			a.stopLocalMITM()
		}
	}
	if p.sys && a.sysOn {
		out, err := sysproxy.Disable(a.sysSaved)
		switch {
		case err != nil:
			errs = append(errs, err)
		case out == sysproxy.NotHeld:
			a.log.Warn("系统代理已被其他程序接管，暂停时未还原（避免覆盖对方的设置）")
		}
		a.sysOn, a.sysSaved = false, nil
	}
	a.paused = p
	a.log.Info("已暂停加速：系统代理、hosts 与反代均已还原", "可恢复", true)
	return errors.Join(errs...)
}

// Paused 报告当前是否处于「一键暂停」状态。
func (a *App) Paused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paused.active()
}

// Resume 恢复「一键暂停」撤掉的那些项。
func (a *App) Resume() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resumeLocked("手动恢复")
}

// resumeLocked 是 Resume 的实现（需持有 a.mu）。
//
// 先作废旧定时器再干活：手动恢复之后，那个「30 分钟后自动恢复」不该再醒来一次
// （它醒来时代号已经对不上，见 autoResume）。trigger 只用于日志，说明这次恢复是谁触发的。
func (a *App) resumeLocked(trigger string) error {
	a.pauseGen++
	a.pauseUntil = time.Time{}
	if a.ln == nil || a.stopped {
		return fmt.Errorf("服务未启动")
	}
	p := a.paused
	if !p.active() {
		a.paused = nil
		return nil
	}
	var errs []error
	// 恢复顺序与暂停相反：先把反代装回来（hosts 需要它把域名指向本地），再接管 hosts，最后系统代理。
	if p.mitm && a.mitmCa == nil {
		ca, err := mitm.LoadOrCreate(config.Dir())
		if err != nil {
			return fmt.Errorf("加载本地 CA 失败: %w", err)
		}
		if installed, _ := mitm.Installed(ca); !installed {
			return fmt.Errorf("根证书已被移除，无法恢复无 SNI 反代；请先安装证书")
		}
		a.configureMITM(ca)
	}
	if p.hosts && !a.hostsOn {
		a.startLocalMITM()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := a.hm.Apply(ctx); err != nil {
			errs = append(errs, fmt.Errorf("重新接管 hosts: %w", err))
		} else {
			a.hostsOn = true
			if a.loopCtx != nil {
				go a.hm.Run(a.loopCtx)
			}
		}
	}
	if p.sys && !a.sysOn {
		if saved, err := sysproxy.Enable(a.proxyAddr()); err != nil {
			errs = append(errs, err)
		} else {
			a.sysSaved, a.sysOn = saved, true
		}
	}
	a.paused = nil
	a.log.Info("已恢复加速", "trigger", trigger)
	return errors.Join(errs...)
}

// disableMITMLocked 关闭无 SNI 反代（需持有 a.mu，不碰配置与日志）。
// 顺序很重要：先把 hosts 里的本地回环指向改回真实 IP，再关本地 :443/:80 监听。
func (a *App) disableMITMLocked() error {
	a.hm.SetLocalHosts(nil)
	a.hm.SetLocalIPv6(false)
	if a.hostsOn && !a.applyHostsLocked("停用无 SNI 反代") {
		// hosts 没改成功：这时绝不能关本地监听，否则托管块里的域名会指向死端口。
		a.hm.SetLocalHosts(a.mitmLocalNames)
		a.hm.SetLocalIPv6(a.px.LocalIPv6Up())
		return fmt.Errorf("停用无 SNI 反代失败：hosts 未能更新（可能仍需要管理员权限），已保持开启状态")
	}
	a.px.DisableMITM()
	a.mitmCa = nil
	a.mitmLocalNames = nil
	a.stopLocalMITM()
	return nil
}

// ConfigPath 返回生效的配置文件路径。
func (a *App) ConfigPath() string {
	if a.cfgPath != "" {
		return a.cfgPath
	}
	return config.DefaultPath()
}

// DataDir 返回配置与证书等数据的目录。
func (a *App) DataDir() string { return config.Dir() }

// applyHostsLocked 重新写入 hosts 托管块（需持有 a.mu），返回是否成功。
//
// 调用方在「关闭某个本地监听」之前必须先确认这一步成功：hosts 里还指着 127.0.0.1
// 而监听已经关掉，那些域名就会一直连接失败（见 SetMITM 的停用路径）。
func (a *App) applyHostsLocked(why string) bool {
	return a.applyHostsLockedTimeout(why, 2*time.Minute)
}

// applyHostsLockedTimeout 是 applyHostsLocked 的可控超时版本（健康自愈用更短的上限）。
func (a *App) applyHostsLockedTimeout(why string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := a.hm.Apply(ctx); err != nil {
		a.log.Warn("hosts 更新失败", "why", why, "err", err)
		return false
	}
	return true
}

// saveConfigLocked 把当前配置持久化到配置文件（需持有 a.mu），失败仅记录日志。
func (a *App) saveConfigLocked() {
	if a.cfg == nil {
		return
	}
	path := a.ConfigPath()
	if err := config.Save(path, a.cfg); err != nil {
		a.log.Warn("保存配置失败", "path", path, "err", err)
	}
}

// sysProxyHeld 报告系统代理是否仍由本实例持有（注册表里还是我们写入的地址）。
// 读注册表失败时不妄下结论——按「仍持有」处理，避免把正常状态误报成「被其他程序接管」。
func (a *App) sysProxyHeld() bool {
	if !a.sysOn {
		return false
	}
	held, err := sysproxy.Held(a.proxyAddr())
	if err != nil {
		a.log.Debug("读取系统代理状态失败，按仍持有处理", "err", err)
		return true
	}
	return held
}

// freeLoopbackAddr 把 "host:port" 变成 "host:0"（保留配置里的回环主机），
// 供「端口被占用时改用空闲端口」使用。
func freeLoopbackAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, "0")
}

// SaveConfig 把当前配置持久化到配置文件（供调用方在改动配置后立即生效）。
func (a *App) SaveConfig() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return config.Save(a.ConfigPath(), a.cfg)
}

// Status 返回当前状态快照。
func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		Running:     a.ln != nil && !a.stopped,
		SystemProxy: a.sysOn && a.sysProxyHeld(),
		Hosts:       a.hostsOn,
		MITM:        a.mitmCa != nil,
		MITMNote:    a.mitmNote,
		MITMLocal:   a.mitmLocal443,
		Services:    append([]string(nil), a.svcIDs...),
		Paused:      a.paused.active(),
		DNS:         a.res.Health(),
		Health:      a.health,
	}
	if a.sysOn && !st.SystemProxy {
		// 我们确实接管过，但注册表里已经不是我们了（Clash 之类顶掉了，或用户手动关掉）
		st.ProxyNote = "已被其他程序接管"
	}
	if st.Paused {
		st.Health.State = HealthPaused
		st.Health.Reason = ""
		if !a.pauseUntil.IsZero() {
			st.PauseUntil = a.pauseUntil.Unix()
			// 向下取整并夹到 0：界面显示「还剩 1 分钟」时不该因为少 200ms 而变成 0。
			if remain := int64(time.Until(a.pauseUntil).Seconds()); remain > 0 {
				st.PauseRemainSec = remain
			}
		}
	}
	if !st.Running {
		st.Health.State = HealthStopped
		st.Health.Reason = ""
	}
	if a.ln != nil {
		st.Listen = a.ln.Addr().String()
	}
	if st.Running {
		st.UptimeSec = int64(time.Since(a.startedAt).Seconds())
	}
	st.Proxy = a.px.Snapshot()
	return st
}

// ProxyAddr 返回配置监听地址（用于系统代理指向）。
func (a *App) ProxyAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxyAddr()
}

func (a *App) proxyAddr() string {
	if a.ln != nil {
		return a.ln.Addr().String()
	}
	return a.cfg.Listen
}

// RecoverStale 清理上一次异常退出（崩溃/强杀）遗留的系统代理与 hosts 状态。
//
// 前提是这些状态**确实没人管了**：状态文件里记的代理地址现在还连得上，就说明建立这轮
// 接管的进程仍然活着（代理端口是它开的）——那不是遗留状态，清了只会让对方的加速静默
// 失效（对方界面照旧显示「已启用」）。多实例/换端口跑的时候尤其容易踩到。
func RecoverStale(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if addr, live := staleOwnerAlive(); live {
		log.Warn("系统状态仍由另一个实例持有，已跳过遗留清理", "代理地址", addr)
		return
	}
	if sysproxy.Active() {
		out, err := sysproxy.Disable(nil)
		switch {
		case err != nil:
			log.Error("清理遗留系统代理状态失败", "err", err)
		case out == sysproxy.NotHeld:
			log.Warn("遗留的系统代理已被其他程序接管，已清除记录但未改动注册表")
		case out == sysproxy.DisabledDeadProxy:
			log.Warn("系统代理状态文件已损坏（原值无法恢复），已关闭指向失效本地端口的系统代理")
		default:
			log.Info("已还原上次遗留的系统代理设置")
		}
	}
	// 仅移除托管块，不涉及解析，无需 resolver
	hm := hosts.New(hosts.DefaultPath(), nil, nil, 0, log)
	if changed, err := hm.Remove(); err != nil {
		log.Error("清理遗留 hosts 托管块失败", "err", err)
	} else if changed {
		log.Info("已移除上次遗留的 hosts 托管块")
	}
}

// addrAlive 报告 addr 上是否已经有进程在监听：用来判断遗留的系统代理状态是否还「活着」。
func addrAlive(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// staleOwnerAlive 报告状态文件里记录的接管是否仍然有效：拿到记录地址，且它还能连上。
// 能连上说明建立这轮接管的进程还在跑（代理端口是它开的），这份状态不该被当作遗留清掉。
func staleOwnerAlive() (string, bool) {
	addr, ok := sysproxy.SavedAddr()
	if !ok || !addrAlive(addr) {
		return "", false
	}
	return addr, true
}

// equalStrings 比较两个字符串列表是否完全相同（顺序敏感：配置里的顺序会体现在界面上）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
