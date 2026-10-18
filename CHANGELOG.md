# 变更记录（CHANGELOG）

本项目遵循[语义化版本](https://semver.org/lang/zh-CN/)。版本号的**唯一权威来源**是仓库根目录的
[`VERSION`](VERSION) 文件，改版本请执行：

```powershell
.\bump.ps1 patch   # 0.0.1 -> 0.0.2   修 bug、小改动、文档整理
.\bump.ps1 minor   # 0.0.2 -> 0.1.0   新增功能、行为有变化
.\bump.ps1 major   # 0.4.0 -> 1.0.0   达到可发布状态
```

`bump.ps1` 会同时改写 `VERSION` 与 `cmd/tongtu/version.go`（`version_test.go` 会检查两者没有漂移），
`build.ps1` 会读取 `VERSION` 并用 `-ldflags -X main.Version=...` 写进 exe。

### Git 标签约定

**只为重要更新版本打 tag**，即功能成型、对外发布、配置或命令行为有里程碑变化的版本（如 `v0.1.0`、`v1.0.0`）；
`0.0.x` 的日常迭代**只提交推送、不打 tag**。打标签用 annotated 形式：`git tag -a vX.Y.Z -m "通途 vX.Y.Z"`。
（仓库里已存在的 `v0.0.3` / `v0.0.4` 是早期「每版都打」留下的，保留即可，新的迭代不再这样。）

## 版本阶段

| 阶段 | 规则 | 承诺 |
| --- | --- | --- |
| `0.0.x` | 开发期。每次有实质变更就抬一位 | 配置格式、命令行为、界面布局都可能随时变，不保证兼容 |
| `0.x.y` | 功能成型但未定稿 | 新增功能抬 minor，修复抬 patch；配置版本号（`config.yaml` 的 `version`）变更时会自动备份升级 |
| `1.0.0` | 可发布 | 配置格式与命令行为向后兼容；破坏性变更必须抬 major 并在本文件写明迁移方法 |

## [未发布]

（无）

## [0.0.5] - 2026-10-18

### 修复

- **hosts 模式不再被「IPv6 优先」绕过**（`internal/proxy`、`internal/hosts`、`internal/app`）：
  之前反代域名在 hosts 里只写 `127.0.0.1`，而**域名自己的 AAAA 记录仍然生效**——Windows / Chromium 走
  Happy Eyeballs 时优先 IPv6，于是客户端连的是真实 IPv6 边缘：既不经过本地 `:443` 接管，又可能正好撞上
  SNI 封锁（`raw.githubusercontent.com` 就是这种域名）。现在本地监听除了 `127.0.0.1:443/:80` 还会尽力绑一份
  `[::1]:443/:80`，绑上时反代域名在 hosts 里**同时写 `::1`**，把该域名的 AAAA 也指向本地；
  绑不上（本机没有 IPv6 回环）就退化为只写 IPv4，行为与之前完全一致。
- **候选 IP 归一化不再把 IPv6 变成空地址**（`internal/proxy`）：`mitmCandidates` 里直接写了 `ip.To4()`，
  而它对 IPv6 返回 `nil`——一旦有 IPv6 候选就会去拨 `"<nil>:443"`。现在只把 4-in-6 压成 IPv4，
  真正的 IPv6 原样保留。

### 变更

- **DoH 全部失败时的系统解析兜底放宽到 IPv4+IPv6**（`internal/resolver`）：`Lookup` 的回退从 `ip4` 改为 `ip`，
  只有 AAAA 记录的域名在系统代理路径上也能拿到可用地址走 IPv6 直连（此前会直接报解析失败）。
- **hosts 模式的「指向优选 IP」条目仍然只写 IPv4**（`LookupPublic` 现在显式滤掉 IPv6）：写进 hosts 的地址
  会成为客户端**首选**（IPv6 优先），而我们没有干净来源校验 AAAA（DoH 查询只取 A 记录），写错比不写更糟。

### 测试

- `internal/hosts`：`TestApplyLocalHostsIPv6`（开 / 关 IPv6 时本地主机名的写入与清理，非本地主机名不受影响）。
- `internal/proxy`：`TestNormalizeCandidateKeepsIPv6`、`TestListenLocalIPv6MarksFlag`。
- `internal/resolver`：`TestPublicV4DropsIPv6AndPrivate`。

### 验证记录（本机实测）

- 非管理员启动（`hosts: true`，写入 hosts 预期被拒），本地监听起在了四个地址上：
  `127.0.0.1:80`、`127.0.0.1:443`、`[::1]:80`、`[::1]:443`；日志 `hosts 直连 MITM 已就绪 domains=8 ipv6=true`。
- 从 PowerShell 对 **`[::1]:443`** 发起真实 TLS 握手（`SslStream.AuthenticateAsClient("steamcommunity.com")`）：
  握手成功（TLS 1.3），证书 `Subject=CN=steamcommunity.com`、`Issuer=CN=Tongtu Local Root CA`；同一条连接上发
  `GET /` 得到 `HTTP/1.1 200 OK`（出站走了无 SNI 反代，日志 `预热完成（无 SNI 反代优选）`）。
  即「IPv6 回环 → 本地解密 → 无 SNI 出站」整条链路可用。
- 收尾：`stop` 后 0 个 tongtu 进程、0 个 443/80 监听、无 `sysproxy.json` / `guard.json`，注册表回到原值。

## [0.0.4] - 2026-10-18

### 新增

- **系统代理被外部接管时如实上报，且退出不覆盖对方的设置**（`internal/sysproxy`、`internal/app`）：
  - 新增 `sysproxy.Held(addr)`；`Status.SystemProxy` 现在表示「**实际仍由本实例持有**」（注册表地址还是我们写入的、
    且代理是开启状态），新增 `Status.ProxyNote` 说明原因。`tongtu status` 显示
    「系统代理: 关（已被其他程序接管）」，窗口状态条 / 托盘 tooltip 同源同步。
  - `sysproxy.Disable` 在还原前先确认「是否还由通途持有」：已被 Clash / v2rayN 之类顶掉、或用户手动关掉了代理时
    **不还原**（返回 `takenOver`），避免把对方的设置覆盖成我们接管前的旧值；退出时在日志里明确说明。
    崩溃恢复（`RecoverStale`）同样走这条判断。
  - 被顶掉后想拿回来：托盘「系统代理」开关点一下就重新接管（以当前值作为新的"原值"），
    不会出现「菜单显示已关、点了却没反应」（`SetMode` 现在比较的是实际持有状态）。
- **端口被占用自动换口**（`internal/control`、`internal/app`、`cmd/tongtu`）：
  - `listen` 被占用 → 改用同主机的空闲端口，系统代理指向实际端口，启动摘要与 `tongtu status` 显示实际端口。
  - `control` 被**别的程序**占用 → 换空闲端口并**写回 `config.yaml`**（否则 `status` / `stop` 连不上本实例）。
  - 新增 `control.ProbeControl`：控制端口上的「占用」不再等同于「tongtu 已在运行」——只有当
    端口被一个形状不对的 HTTP 服务占住时才判定为「别的程序」；被非 HTTP 程序占住时保守按通途处理
    （宁可误报也不误起第二个实例）。`Running` 保持原样（它还被用来探测**代理端口**的存活，
    那里没有 HTTP 控制 API，改成 HTTP 探测会打到自己身上）。

### 变更

- **hosts 默认刷新周期 30 分钟 → 10 分钟**（`internal/config`、`internal/hosts`），且**刷新失败后改为每分钟重试**，
  成功再回到正常周期——一次 DoH 抖动不再让托管块陈旧一整个周期。已有 `config.yaml` 里的
  `hosts_refresh_minutes` 保持不变（只影响新配置），想改小请手工编辑。

### 测试

- `internal/sysproxy`：`TestHeldIn`（接管地址 / 别的程序 / 用户关掉 / 字段缺失 / 空值）。
- `internal/control`：`TestProbeControlNoListener`、`TestProbeControlForeignHTTP`、`TestProbeControlTongtu`、
  `TestProbeControlNonHTTP`、`TestFreeLoopbackAddr`、`TestFreeLoopbackAddrBadInput`。
- `internal/hosts`：`TestRunRetriesQuicklyAfterFailure`（失败后短周期重试，且随 ctx 退出）。
- `internal/proxy`：修掉 `TestWarmHonorsCancel` 的偶发失败——`select` 在两个 case 都就绪时随机挑，
  已取消的预热可能仍派发一个 goroutine 出去；`Warm`/`warmOne` 现在先看 `ctx.Err()`，取消后不发任何请求。

### 验证记录（本机实测）

- 占用 `127.0.0.1:19180`（裸监听）与 `127.0.0.1:19181`（另一个程序的 HTTP 服务）后启动：
  - 日志：`控制端口已被其他程序占用，改用空闲端口 配置=127.0.0.1:19181 实际=127.0.0.1:65410`、
    `代理端口被占用，已改用空闲端口 配置=127.0.0.1:19180 实际=127.0.0.1:65411`；启动**没有**失败，
    也没有误报「tongtu 已在运行」。
  - `config.yaml` 里 `control` 被写回 `127.0.0.1:65410`；`tongtu status` 显示 `代理监听: 127.0.0.1:65411`、
    `系统代理: 开`（即系统代理指向实际端口），`stop` 照常找得到实例。
- 运行期用 `reg add` 把 `ProxyServer` 改成 `127.0.0.1:7890`（模拟别的工具接管）：
  - `tongtu status` → `系统代理: 关（已被其他程序接管）`；
  - `tongtu stop` → 日志 `系统代理已被其他程序接管，退出时未还原（避免覆盖对方的设置）`，
    注册表仍为 `ProxyEnable=1 ProxyServer=127.0.0.1:7890`（**未被**还原成接管前的值）；
  - 收尾：无残留进程、无 `sysproxy.json` / `guard.json`，注册表恢复原值。

## [0.0.3] - 2026-10-18

### 修复

- **系统代理的「跳过代理」列表不再被整段替换**（`internal/sysproxy`）：接管期间改为把
  `localhost;127.0.0.1;::1;<local>` **合并**进用户原有的 `ProxyOverride`。此前运行期间用户配置的
  内网绕过项（如 `192.168.*;10.*;*.corp.local`）会被顶掉，这些流量被硬塞进本地代理绕一圈。
- **剥离上游 `Alt-Svc` 响应头**（`internal/proxy`）：hosts 模式下反代域名指向 `127.0.0.1`，而本地只
  监听 TCP；浏览器读到 `Alt-Svc` 会去尝试注定失败的 QUIC(UDP:443)，白等几秒。

### 新增

- **启动预热**（`internal/proxy.Warm` + `app.Start`）：启动后在后台对 hosts 名单与反代域名做
  预解析/预拨号，命中结果写进与真实请求**共用**的优选缓存；预热与真实请求复用同一套候选来源与
  优选缓存（`dialOneNoSNI`），因此结果直接生效。预热尽力而为、可随退出取消，失败只记日志。
- **反代未启用的原因会直接显示**：新增 `app.Status.MITMNote`，`tongtu status`、启动摘要都会写明
  「缺根证书，执行 `tongtu cert install` 后重启」。此前开机自启（非交互）时证书装不上只会留一条
  日志，用户看到的是「反代莫名不工作」。

### 测试

- `internal/sysproxy`：`TestMergeBypassKeepsUserEntries`、`TestMergeBypassDedupes`、
  `TestMergeBypassAllPresent`、`TestMergeBypassNil`、`TestSplitBypass`。
- `internal/proxy`：`TestWarmPrefillsGoodIP`、`TestWarmNonMITMHostWarmsResolver`、
  `TestWarmHonorsCancel`、`TestPlainHTTPStripsAltSvc`、`TestMITMStripsAltSvc`。

### 验证记录（本机实测）

- 系统代理：运行期 `ProxyOverride` = 用户原值（`localhost;127.*;192.168.*;10.*;172.16.*…;<local>`）
  + `127.0.0.1;::1`；停止后逐字还原，无 `sysproxy.json` / `guard.json` 残留、无残留进程。
- 预热：`log.level=debug` 下 27 个主机名全部完成预热；反代域名写入优选 IP
  （`github.com` → 20.205.243.166、`gist.github.com` → 140.82.114.3、`store.steampowered.com` → 23.x）。
- 延迟：`store.steampowered.com` 冷启动首次 3.4 s → 预热后 0.89 s；`steamcommunity.com` 0.77 s（均 200）。
- 响应头：反代路径已无 `Alt-Svc`。
- 非交互缺证书：独立 `%APPDATA%` 启动 → 日志给出「执行 tongtu cert install 后重启通途」，
  `tongtu status` 显示「无SNI反代: 关（缺根证书，执行 tongtu cert install 后重启）」。

## [0.0.2] - 2026-10-18

### 新增

- `tools/crash-test.ps1`：崩溃防护验收脚本——快照当前网络设置 → 启动 → `taskkill /F` 强杀主进程 →
  断言系统代理 / hosts / `guard.json` / 残留进程都在时限内清理干净 → 打印检查表并还原快照。
  它会区分「环境限制」（监护进程被作业对象一起带走）与「代码问题」，后者才需要改代码。
- 单测：`TestReleaseGuardOnlyOwnToken`、`TestRecycleStaleGuard`、`TestGuardRow`、
  `TestMarkBreakawayFailed`、`TestTooltipText`、`TestShortBytes`。

### 验证记录（本机实测）

- 崩溃防护：启动后 `taskkill /F` 强杀主进程 → 监护进程约 1 秒内完成还原（`guard.log` 依次记录
  「父进程已退出，检查是否需要还原」「开始还原系统代理与 hosts」「还原完成」），
  无残留进程、无 `guard.json`；优雅退出时本轮令牌被正常回收。
- Steam：`store.steampowered.com` 经反代 HTTP 200（0.8s，整页 987KB）；
  `steamcommunity.com` / `www` / `help.steamcommunity.com` 分别 200 / 302 / 400；
  `login.steampowered.com` 保持隧道 302（未被通配误伤）；`avatars.steamstatic.com` 反代后正常。

## [0.0.1] - 2026-10-18

首个纳入版本管理的版本。此前的工作（双模式接管、无 SNI 反代、自绘窗口与彩色日志、崩溃防护等）
已包含在本版基线内。

### 新增

- 托盘「以管理员身份重启」：先还原系统代理与 hosts 再退出，随后弹 UAC 以管理员权限重启，
  Hosts 模式随之生效（免去「手动退出 → 右键以管理员身份运行」）。
- 托盘悬停提示改为实时状态：`运行中 · 系统代理✓ · Hosts✗ · 反代✓ · ↓↑ 流量`，不打开窗口也能看状态。

### 修复

- **Steam 商店与社区打不开**：`store.steampowered.com` 在境内直连会被 SNI 阻断
  （实测超时，或只收到十几 KB 就断流），而它原先不在无 SNI 反代名单里。
  现在服务规则自带「必须走反代」的域名与实测可用兜底 IP：
  `store.steampowered.com`、`steamcommunity.com`（含 `www`/`help`）、`avatars.steamstatic.com`
  （头像直连会被 RST）。注意**不能**直接写 `*.steampowered.com`：
  实测 `login.steampowered.com` 剥离 SNI 后握手直接失败，会被一起拖下水。

### 改进

- 崩溃防护：优雅退出时主动回收监护令牌，不再在数据目录里留下陈旧的 `guard.json`。
- 崩溃防护：启动时自动回收「主人已经不在」的陈旧令牌，并写进 `guard.log` 便于排查。
- 崩溃防护：监护进程因作业对象限制而无法脱离主进程（异常退出时会被一起带走）时，
  日志与启动摘要都会明确告警「崩溃防护受限」，不再让人误以为有保护。

### 工程

- 版本管理：新增 `VERSION`、`bump.ps1`、本变更记录；`build.ps1` 自动读取版本号；
  新增 `version_test.go` 防止 `VERSION` 与代码里的默认值漂移。
- 反代名单与兜底 IP 补充进 `config.Default()`：老配置在加载时会自动合并新增的内置项
  （保留用户已有值），因此不需要手动改配置文件。
- `tongtu test` 新增「直连HTTPS」与「无SNI反代」两列，并补上 `www`/`help`/`avatars`/`login`
  等域名：以后遇到「某个页面打不开」，一条命令就能看出是解析、直连还是反代的问题。
