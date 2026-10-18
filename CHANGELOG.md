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

## 版本阶段

| 阶段 | 规则 | 承诺 |
| --- | --- | --- |
| `0.0.x` | 开发期。每次有实质变更就抬一位 | 配置格式、命令行为、界面布局都可能随时变，不保证兼容 |
| `0.x.y` | 功能成型但未定稿 | 新增功能抬 minor，修复抬 patch；配置版本号（`config.yaml` 的 `version`）变更时会自动备份升级 |
| `1.0.0` | 可发布 | 配置格式与命令行为向后兼容；破坏性变更必须抬 major 并在本文件写明迁移方法 |

## [未发布]

（无）

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
