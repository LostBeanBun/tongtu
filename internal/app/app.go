// Package app 编排 tongtu 的核心运行时：本地代理服务 + 系统代理/hosts 双模式。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	Running     bool              `json:"running"`
	Listen      string            `json:"listen"`
	UptimeSec   int64             `json:"uptime_sec"`
	SystemProxy bool              `json:"system_proxy"`
	Hosts       bool              `json:"hosts"`
	MITM        bool              `json:"mitm"`       // 无 SNI 反代是否启用
	MITMLocal   bool              `json:"mitm_local"` // hosts 直连用本地 :443 是否就绪
	Services    []string          `json:"services"`
	Proxy       proxy.Snapshot    `json:"proxy"`
	DNS         []resolver.Health `json:"dns"`
}

// Options 由调用方注入 App 所需依赖。
type Options struct {
	Config     *config.Config
	Rules      *rules.Set
	SysProxy   bool     // 是否接管系统代理
	HostsMode  bool     // 是否接管 hosts
	MITMCA     *mitm.CA // 无 SNI 反代根证书；nil 表示不启用
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
	mitmLocalNames []string // 命中反代名单、需写 127.0.0.1 的 hosts 主机
	mitmLocal443   bool
	mitmLocal80    bool

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
	}
	// 无 SNI 反代：SNI 封锁域名本地解密、出站剥离 SNI（候选 IP 来自干净 DoH）
	if opts.MITMCA != nil {
		a.configureMITM(opts.MITMCA)
	}
	return a
}

// configureMITM 装配无 SNI 反代（初始化阶段或持有 a.mu 时调用）。
func (a *App) configureMITM(ca *mitm.CA) {
	a.px.EnableMITM(ca, func(host string) bool {
		return rules.MatchPatterns(host, a.cfg.MITM.Domains)
	})
	a.mitmCa = ca
	a.mitmLocalNames = a.mitmLocalNamesFor(a.rs)
}

// mitmLocalNamesFor 返回「命中反代名单」的 hosts 主机名（这些需写 127.0.0.1）。
func (a *App) mitmLocalNamesFor(rs *rules.Set) []string {
	if rs == nil {
		return nil
	}
	var out []string
	for _, h := range rs.Hosts() {
		if rules.MatchPatterns(h, a.cfg.MITM.Domains) {
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
		return fmt.Errorf("监听 %s 失败: %w", a.cfg.Listen, err)
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
	return nil
}

// startLocalMITM 启动 hosts 直连用的本地 MITM 监听（需持有 a.mu）。
// :443 绑定成功后，反代域名在 hosts 中写 127.0.0.1；失败则降级为真实 IP。
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
	if err := a.px.ListenLocal80(); err != nil {
		a.log.Debug("本地 :80 监听失败（HTTP 直连转发不可用，不影响 HTTPS）", "err", err)
	} else {
		a.mitmLocal80 = true
	}
	a.log.Info("hosts 直连 MITM 已就绪", "domains", len(a.mitmLocalNames))
}

// stopLocalMITM 关闭本地直连监听（需持有 a.mu）。
func (a *App) stopLocalMITM() {
	if !a.mitmLocal443 && !a.mitmLocal80 {
		return
	}
	a.px.CloseLocal()
	a.mitmLocal443, a.mitmLocal80 = false, false
	a.hm.SetLocalHosts(nil) // 还原为真实 IP 解析
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
		if err := sysproxy.Disable(saved); err != nil {
			errs = append(errs, err)
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
func (a *App) SetMode(sys, hostsWant bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil {
		return fmt.Errorf("服务未启动")
	}
	var errs []error

	sysChanged := sys != a.sysOn
	hostsChanged := hostsWant != a.hostsOn

	if sysChanged {
		if sys {
			saved, err := sysproxy.Enable(a.proxyAddr())
			if err != nil {
				errs = append(errs, err)
			} else {
				a.sysSaved = saved
				a.sysOn = true
			}
		} else if err := sysproxy.Disable(a.sysSaved); err != nil {
			errs = append(errs, err)
		} else {
			a.sysOn = false
			a.sysSaved = nil
		}
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
	return errors.Join(errs...)
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
		a.log.Info("无 SNI 反代已启用")
	} else {
		a.px.DisableMITM()
		a.mitmCa = nil
		a.mitmLocalNames = nil
		a.cfg.MITM.Enabled = false
		a.stopLocalMITM()
		if a.hostsOn {
			a.applyHostsLocked("停用无 SNI 反代")
		}
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil {
		return fmt.Errorf("服务未启动")
	}
	a.rs = rs
	a.svcIDs = append([]string(nil), ids...)
	a.cfg.Services = append([]string(nil), ids...)
	a.px.SetRules(rs)
	a.hm.SetHosts(rs.Hosts())

	// 反代本地名单随服务组变化
	if a.mitmCa != nil {
		a.mitmLocalNames = a.mitmLocalNamesFor(rs)
		a.hm.SetLocalHosts(a.mitmLocalNames)
	}
	if a.hostsOn {
		a.applyHostsLocked("切换加速服务")
	}
	a.saveConfigLocked()
	a.log.Info("加速服务已切换", "services", a.svcIDs)
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

// applyHostsLocked 重新写入 hosts 托管块（需持有 a.mu），失败仅记录日志。
func (a *App) applyHostsLocked(why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := a.hm.Apply(ctx); err != nil {
		a.log.Warn("hosts 更新失败", "why", why, "err", err)
	}
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

// Status 返回当前状态快照。
func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		Running:     a.ln != nil && !a.stopped,
		SystemProxy: a.sysOn,
		Hosts:       a.hostsOn,
		MITM:        a.mitmCa != nil,
		MITMLocal:   a.mitmLocal443,
		Services:    append([]string(nil), a.svcIDs...),
		DNS:         a.res.Health(),
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
		if err := sysproxy.Disable(nil); err != nil {
			log.Error("清理遗留系统代理状态失败", "err", err)
		} else {
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
