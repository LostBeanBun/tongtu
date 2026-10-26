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
（`v0.0.2` / `v0.0.3` / `v0.0.4` 是早期「每版都打」留下的，保留即可；从 `v0.1.0` 起只在重要版本打 tag。）

## 版本阶段

| 阶段 | 规则 | 承诺 |
| --- | --- | --- |
| `0.0.x` | 开发期。每次有实质变更就抬一位 | 配置格式、命令行为、界面布局都可能随时变，不保证兼容 |
| `0.x.y` | 功能成型但未定稿 | 新增功能抬 minor，修复抬 patch；配置版本号（`config.yaml` 的 `version`）变更时会先备份、再保留用户设置升级 |
| `1.0.0` | 可发布 | 配置格式与命令行为向后兼容；破坏性变更必须抬 major 并在本文件写明迁移方法 |

## [0.6.0] - 2026-10-26

> 主题：**跑着跑着坏了，能自己好**。0.5.0 解决了「能不能装、能不能升级」，
> 这一版处理更难缠的一类问题：进程还在、界面还显示「运行中」，但网络已经不通了。
> 同时补上睡眠唤醒、换 Wi-Fi / 插网线之后的自动恢复，以及无 SNI 反代的自动化测试安全网。

### 新增

- **假活自愈**：新增健康状态机（`ok` / `degraded` / `healing` / `paused` / `stopped`），
  每 15 秒检查一次「本地代理监听、hosts 托管块、本地 `:443` 监听、DoH 端点状态、系统代理归属」。
  发现可自愈的问题就当场修：重新绑定代理监听（端口被占用时自动换空闲端口并同步系统代理）、
  重写 hosts 托管块、重置 DNS 缓存与端点顺序并重新预热。
  系统代理若已被 Clash / v2rayN 等程序接管，**只标记降级并写明原因，不抢回、不覆盖**。
  `tongtu status`、复制诊断与窗口状态条都会显示健康度、自愈次数与降级原因。
- **睡眠 / 换网恢复**：窗口收到 `WM_POWERBROADCAST` 的自动恢复 / 用户恢复 / 严重休眠恢复
  事件时立即触发；跨平台侧另有网络指纹轮询（接口名、状态、MTU、非回环地址的哈希），
  换 Wi-Fi、插网线、VPN 开关、DHCP 换地址都会触发同一套恢复。恢复动作串行化并做 3 秒防抖，
  同一次事件引发的多个消息只真正执行一遍；DNS 全失败时的重置另有 2 分钟冷却，避免断网期间空转。
- **MITM 自动化测试安全网**：为 `internal/mitm` 补齐 CA 生成/加载/损坏恢复、证书链与
  DNSNames、指纹、并发签发缓存、安装确认与拒绝路径测试；为 `internal/proxy` 补齐
  无 SNI 反代的 CONNECT + 本地解密、SNI 缺失/名单外拒绝、Host 头路由、请求透明转发、
  上游 502、IPv6 候选、候选过滤/去重、优选缓存与 HTTP/2 禁用测试。全部使用本地 TLS
  upstream 与假解析器，不依赖真实外网。

### 变更

- **网络状态可整体作废**：`resolver.Reset()` 清 DNS 缓存、恢复端点顺序、清故障标记、
  失效远程 hosts；`proxy.ResetNetwork()` 清优选 IP 缓存并关闭空闲连接。远程 hosts 增加
  generation 机制，换网前发出、换网后才返回的旧抓取结果会被丢弃，不会把旧 IP 写回来。
- **系统代理换口**：新增 `sysproxy.Retarget()`，代理监听换端口时只在本实例仍持有系统代理的
  前提下更新注册表与状态文件，并保留接管前的原值；`Enable` / `Disable` / `Retarget` / `Held`
  统一串行化，避免健康自愈与退出流程交错执行留下半成品。
- **健康检查不访问外网**：只查本地监听、hosts 托管块、DNS 端点状态与系统代理归属，
  不会因为健康检查本身产生对外流量；hosts 重写失败（如非管理员）或 `:443` 长期被占用时，
  同类自愈动作有 1 分钟冷却，避免每 15 秒重试并刷日志。

### 验证

- 新增测试：健康状态机、监听丢失后自动重绑、恢复防抖与重复触发、未启动时恢复为空操作、
  DNS 全失败判定、网络指纹稳定性与地址变化敏感性；`resolver.Reset` 清缓存/恢复端点顺序、
  远程 hosts 失效后重新抓取、`proxy.ResetNetwork` 清优选缓存、`sysproxy.retargetPlan`
  只改仍由本实例持有的注册表值、窗口电源消息触发与去重。
- 本地通过：`gofmt`、`go vet ./...`、staticcheck、`go test ./... -count=1`、
  `GOOS=linux go build ./...`；关键包 `go test ./internal/app ./internal/proxy ./internal/resolver
  ./internal/sysproxy ./cmd/tongtu -count=30` 无 flake。
- MITM 测试全部使用本地 TLS upstream 与假解析器，CI 的 Windows 完整测试与 Linux `-race`
  覆盖本机无法运行的部分（本机无 gcc / WSL）。
- CI 实跑全绿（run `40257927262`，提交 `1d3b56d`）：格式与静态检查、Windows 完整测试、
  Linux `go test -race` 均通过。过程中修掉一个只在 Windows CI 暴露的测试竞态：
  `healthLoop` 的首次健康检查可能先于用例断言完成自愈，前置条件改为接受
  「端口已释放」或「后台已完成自愈」两种路径。
- Release 实跑成功（run `40258184336`，tag `v0.6.0` → `4275ab4`）：质量门、构建、exe 版本自检、
  打包与发布全部通过。`releases/latest` 已指向 v0.6.0；`tongtu-0.6.0-windows-x64.exe`
  （10,026,496 字节）SHA-256 为
  `c8b9172af52d92cc032d0b80559d8da5891636c73a4ee98762780139b1dae4a2`，
  与 Release 附件 `SHA256SUMS.txt` 一致（通过 GitHub 资产 digest 交叉核对）。

## [0.5.0] - 2026-10-23

> 主题：**把「能升级」做成闭环**。0.4.0 做了检查更新，但仓库里没有任何 Release，
> 点了也只能看到「仓库还没有发布版本」。这个版本补上发布流水线，并顺手修掉一个
> 以后一定会咬人的问题：配置版本升级会把用户设置整体重置。

### 新增

- **CI 质量门**（`.github/workflows/ci.yml`）：每次 push / PR 自动跑
  gofmt、`go vet`、staticcheck、Windows/Linux 交叉编译；测试分两个平台——
  Windows 跑完整测试（窗口、注册表、证书相关），Linux 跑 `go test -race`。
  在此之前这些检查全靠「记得在本地跑一遍」。
- **Release 流水线**（`.github/workflows/release.yml`）：推送 `vX.Y.Z` tag 后自动
  校验 tag 与 `VERSION` 一致 → 跑质量门 → `build.ps1` 构建 → 生成
  `tongtu-X.Y.Z-windows-x64.exe` 与 `SHA256SUMS.txt` → 用 CHANGELOG 条目生成发布说明 →
  创建 GitHub Release。重复执行会覆盖附件与说明，不会因为 Release 已存在而红掉。
  发布前还会运行一次构建出的 exe 并核对版本号，防止「tag 对了、exe 里版本没进去」。
- **`tools/release-notes.ps1`**：从 CHANGELOG 提取指定版本条目，并在开头附上下载与
  校验说明；本地也能先预览发布说明。
- README 增加 CI 徽章与「发布与升级」一节：下载、`Get-FileHash` 校验、退出后覆盖 exe 的完整步骤；
  USAGE 增加「升级」小节。

### 变更

- **配置升级不再重置用户设置**：旧版配置仍会备份为 `config.yaml.vN.bak`，但升级方式从
  「按新默认值重建」改为「新默认值打底、旧文件覆盖」——你改过的端口、服务组、
  自定义站点、日志设置都会保留，只有缺失字段取新默认值。语义确实变化的值由新增的
  `migrate` 逐版本定点修正（本次修正了 `223.5.5.5` 的 DoH 路径 `/dns-query` → `/resolve`）。
- **配置文件版本高于程序时明确拒绝**：用旧版程序打开新版配置会直接报错并提示升级通途，
  而不是按旧结构读一遍再写回去、悄悄丢掉新字段。
- **配置写入改为原子写**（临时文件 + 改名）：写一半被强杀或蓝屏时不会留下半截 YAML，
  避免下次启动因为配置损坏直接失败。

### 验证

- 新增配置测试：无版本号旧配置、带版本号旧配置、未来版本配置、覆盖写入与临时文件清理；
  确认用户端口 / 服务组 / 自定义站点 / 日志级别保留，错误端点被定点修正，缺失字段取新默认值。
- `gofmt` / `go vet` / `staticcheck` / `go test ./...` / `GOOS=linux go build ./...` 全绿。
- 两个 workflow 用 YAML 解析器校验通过；`release-notes.ps1 -Version 0.4.0` 本地预览正常。
- CI 实跑全绿（run `39882488577`）：格式与静态检查、Windows 完整测试、Linux `go test -race` 均通过。
  过程中修掉两处只在 CI 暴露的问题：staticcheck 在 Linux 下把 Windows 专用符号误报为 U1000
  （改为按 Windows 目标平台检查）；非 Windows 的 `processAlive` 恒为 false，导致监护令牌
  回收测试误判（改为按 Unix 的 signal 0 语义判断进程存活）。
- 首次 Release 实跑成功（run `39885561462`，tag `v0.5.0` → `6023468`）：质量门、构建、
  打包与创建 Release 全部通过。`releases/latest` 已指向 v0.5.0；下载的
  `tongtu-0.5.0-windows-x64.exe`（9,973,248 字节）SHA-256 为
  `a4c21b41d921d30f62e4bda71b00f0122b2af2be464d68d274c6c5daf877f31f`，
  与 Release 附件 `SHA256SUMS.txt` 一致。
- 修掉两个 Windows UI 测试 flake：合成 `WM_MOUSEMOVE` 时真实光标在窗口外，
  `TrackMouseEvent` 会立刻投递 `WM_MOUSELEAVE` 把悬停状态清掉（改为只在真实光标位于
  客户区时跟踪离开）；`TestWindowProcHandlesSessionEnd` 未清理关机去重时间戳，
  会让后续用例在 3 秒内被误判成「同一次关机」。

## [0.4.0] - 2026-10-22

> 主题：**跑得久、坏得少、能升级、能卸干净**。前三个版本在解决「能不能用、好不好用」，
> 这个版本回答的是难听但更重要的问题：突然断电/蓝屏之后，系统代理、hosts 会不会被丢在半路？
> 连续跑一天会不会越吃越多？装完怎么干净地拿走？

### 新增

- **断电自愈**：接管系统期间，在 `HKCU\...\RunOnce` 登记 `TongtuRecover`，值就是
  `"<exe>" __recover`；监护进程每 1.5 秒巡检一次，被别的东西删掉就补回来（值相同不写注册表，
  避免无意义的磁盘写入）。下次登录时 `__recover` 会把上一次没有正常退出而残留的系统代理、
  hosts、`guard.json` 收干净，然后**自己清掉 RunOnce**——只兜底一次，不留常驻项。
  暂停、手动关掉接管、正常退出（含 `tongtu stop`、监护进程还原）都会立即撤销它。
  `__recover` 是幂等的：如果检测到还有活着的持有者（例如只是界面崩了、服务还在跑），
  它什么都不做。
- **检查更新（只查不下载）**：托盘菜单新增「检查更新」，命令行是 `tongtu version --check`。
  走 GitHub Releases API，显示「已是最新 / 发现新版本 vX.Y.Z」；有新版本时问一句，
  点是就用浏览器打开 Release 页面。**不做自动下载替换**：通途是绿色单文件，
  在 Windows 上替换正在运行的 exe 不可靠，悄悄失败比不升级更糟。
  遇到「仓库还没有发布版本」、限流、网络错误都会单独说明，不会笼统报「检查失败」。
- **`GET /ui/layout`**（控制端口）：返回自绘控件（尤其是动作区按钮）的矩形与 `id`、`label`。
  这是给 `tools/ui-smoke.ps1` 用的：像素坐标悬停会被状态条流量计数刷新带偏
  （实测确实会点到空白处），改成「先读几何、再按 `id=pause` 精确点击」。
  顺带让脚本能断言按钮文案在 `暂停` / `恢复` 之间翻转，而不只是看日志。
- **`tools/soak-test.ps1`**：长时间运行浸泡测试（默认 24 小时）。按固定间隔采样
  内存 / 句柄 / 线程 / CPU / 本地代理连接数 / 日志轮转情况 / 接管状态与 RunOnce，
  边跑边写 CSV，结束时判断「有没有异常增长」，并正常停止实例、还原系统代理与 hosts。
  `-NoHosts` / `-NoMITM` 可只浸泡部分路径。
- **`tools/uninstall.ps1`**：按顺序撤销进程、开机自启、RunOnce、本地根证书、
  系统代理残留、hosts 托管块；`-Purge` 删数据目录、`-RemoveExe` 删 exe。
  默认只撤销系统改动、保留 exe 与配置（误删配置的代价比多点一次高）。

### 修复

- **卸载脚本误报「移除根证书失败」**：通途是 GUI 子系统程序（双击不弹黑框的前提），
  用 `& $exe ...` 调用时 PowerShell 不会等它结束、`$LASTEXITCODE` 也不会被赋值，
  于是「证书其实已经删掉了」被当成失败。改用 `Start-Process -Wait -PassThru`
  重定向输出到临时文件，同时拿到真实退出码与输出，失败时也能把原因打出来。

### 验证

- 真机 `tools/recover-test.ps1`：启动 → 杀监护进程 → 杀主进程 → 执行 `__recover` →
  断言系统代理已关、RunOnce 已清、`guard.json` 已回收、hosts 无托管块、无进程残留；
  重复执行幂等，一次通过。
- 真机 `tools/ui-smoke.ps1` 全绿：按 `/ui/layout` 点 `暂停` → `ProxyEnable 1→0`、托管块 `2→0`、
  RunOnce 撤下、按钮文案变 `恢复`；再点 → 反向恢复、RunOnce 重新登记；右键「复制代理地址」、
  配置热生效、`×` 只隐藏窗口均通过。
- `tools/soak-test.ps1 -Hours 0.05 -IntervalSec 10` 短跑通过：18 个样本，内存约 +2.5 MB、
  句柄 +54 后走平（启动期预热，不是线性增长），CSV 正常落盘，结束时系统状态干净。
- `tools/uninstall.ps1` 真机跑了两轮（无实例 / 有实例接管中），第二轮起全绿：
  正常停止实例、还原系统代理与 hosts、移除证书；随后 `cert install` 恢复本机证书。
- `tongtu version --check` 真机连通 GitHub API（仓库暂无 Release，正确显示「仓库还没有发布版本」）。
- `gofmt` / `go vet` / `staticcheck` / `go test ./...` / `GOOS=linux go build ./...` 全绿；
  新增测试覆盖更新检查（新版本 / 同版本 / 404 / 403 限流 / 5xx / 坏 JSON / 版本比较）、
  `/ui/layout`（正常、405、nil 时 404）、暂停后的几何快照（`id=pause` 文案翻转）。

## [0.3.0] - 2026-10-22

> 主题：**能加速自己的站点**。往 `config.yaml` 里加一行域名、保存，就生效——不用重启，
> 也不用动代码；想临时全关也只点一下。

### 新增

- **自定义站点**（配置里的 `custom:` 段）：

  ```yaml
  custom:
    domains: ['my.site.com', '*.example.com']   # 走系统代理模式（白名单）
    mitm_domains: ['blocked.site.com']          # 额外走无 SNI 反代
  ```

  规则在加载时规范化（转小写、去空白、去重）；写错（带 `https://`、端口、路径，
  或通配位置不对）会直接报错告诉你哪条不对，而不是存进去以后永远匹配不上。
  通配只进系统代理白名单——hosts 没法枚举通配，硬写进去只会得到一个无意义的主机名。
- **改完保存即生效**：每 2 秒看一次 `config.yaml`（修改时间 + 大小），变了就重新加载，
  把「服务组 / 自定义站点 / 反代域名」应用到正在运行的实例，日志里写一条「配置已热生效」。
  端口、DNS 端点、日志级别这些要重建组件的字段仍需重启——文档里写明，界面不撒谎。
  编辑器正在写文件时会等文件稳定再读；读出来的配置有错只报一次、继续用原来的规则。
- **一键暂停 / 恢复**：窗口动作区最左的 `暂停` 按钮、托盘菜单最上方的「暂停全部加速」，
  把系统代理、hosts、无 SNI 反代一次全撤掉，进程与窗口继续运行；再点一次按**原来的配置**
  原样恢复（不是「重新猜」）。暂停时状态条写「已暂停」、托盘提示同步，
  并且不会再拿「重新接管系统代理」这类待办催你——刚关掉的东西不该被当成故障。
- **复制代理地址**：日志区右键与托盘菜单各一项，复制 `http://127.0.0.1:19180` 形式的地址，
  方便填进别的程序。
- **运行时长**：状态条第二行末尾、托盘悬停提示与「复制诊断信息」里都能看到「已运行 3 小时 20 分」。
- `tongtu status` 遇到暂停中的实例写「已暂停 (… 可恢复 …)」，脚本里 grep 状态不会把暂停误判成停止。
- 剪贴板实现抽到 `internal/clipboard`，窗口与托盘共用一份。

### 修复

- **日志区右键菜单从来没弹出来过**（0.2.0 起的隐患）：`WM_CONTEXTMENU` 的子窗口句柄在
  `wParam`、坐标在 `lParam`，代码却拿 `lParam` 去比子窗口，判定永远不成立。真机右键一次就露馅。
  顺带改用 `lParam` 里的坐标定位菜单（键盘唤起时为 -1，退回鼠标位置）。
- **结果提示会被上一条提示的定时器吞掉**：`SetTimer` 用同一个 id，旧提示的 `WM_TIMER`
  如果正好落在新提示发出之后，会把新提示一起清掉（「正在暂停…」之后那句「已暂停」偶发消失，
  6 次全量测试挂 1 次才被抓到）。现在只清真正到点的那条，没到点就按剩余时间重挂。
- **自检结果晚于「体检结束」**：原来先清「体检中」标记、再写结果，按钮已经回到「自检」
  而结果还没出现；现在写完结果才收尾，两个信号是同一件事。

### 验证

- 真机：点 `暂停` → 注册表 `ProxyEnable 1→0`、hosts 托管块 `2→0`、日志
  「已暂停加速：系统代理、hosts 与反代均已还原 可恢复=true」；再点 `恢复` →
  `ProxyEnable 0→1`、托管块 `0→2`、日志「已恢复加速」。
- 真机：日志区右键 → 菜单 → `复制代理地址`，剪贴板得到 `http://127.0.0.1:19180`。
- 真机：往 `config.yaml` 加 `custom.domains: ['example.com']` 并保存 →
  日志「配置已热生效 services="[steam github]" 自定义域名=1 反代域名=6」，hosts 条目 27→28。
- 浅 / 深两套快照 PNG 复核布局（7 个动作按钮，`暂停` 在最左，不与详情行重叠）。
- `gofmt` / `go vet` / `staticcheck` / `go test ./...` / `GOOS=linux go build ./...` 全绿；
  新增测试覆盖自定义规则（规范化 / 拒绝非法写法 / 匹配与 hosts）、热加载（变更与幂等）、
  暂停记录（部分恢复、无接管时的空操作）、暂停按钮（执行 / 重复点击 / 失败反馈 / 文案切换）、
  提示定时器竞争、日志区右键目标、待办在暂停中保持安静、`humanUptime`、托盘提示。

## [0.2.0] - 2026-10-22

> 主题：**不用命令行**。装证书、体检、看「还差什么」、找日志，全在窗口里点得动。

### 新增

- **窗口自检**：动作区新增 `自检`，点一下就在日志卡片里逐个列出加速域名的**干净解析 / 直连 /
  经代理**三项结果，末尾一行合计（可用 / 仅直连 / 不通），结论同时显示在状态条；体检进行中
  按钮进入按下态、重复点击只提示不会再起一次。逻辑抽到新包 `internal/selfcheck`，
  与 `tongtu test` 共用同一份判定（结论列、失败原因、汇总），窗口只负责渲染
  （`cmd/tongtu/selfcheck_view.go`）。
- **根证书一键安装**：开启「无SNI反代」前先查证书，没装就问一句，点「是」装好后**直接打开反代**。
  窗口与托盘两个入口走同一段代码（`internal/mitm.EnsureInstalled`），
  「去终端执行 `tongtu cert install`」不再是必经之路。文案统一用 `mitm.InstallPrompt`。
- **待办**：动作区最左侧出现 `待办 N`（`>0` 才出现），点开是本机确实没配好的事——
  装根证书 / 以管理员身份重启（让 Hosts 生效）/ 开启开机自启 / 夺回被别的程序接管的系统代理，
  每条都带一句「为什么」，点一下即处理，处理完数字立刻重算。判断要问证书存储与计划任务
  （起进程），因此放在后台按 30 秒兜底刷新，不进 3 秒一次的状态轮询。
- **日志默认落盘**：默认写 `%APPDATA%\tongtu\tongtu.log`，超过 5 MB 轮转、保留 3 份
  （`cmd/tongtu/logfile.go`）—— 双击启动遇到问题时有东西可查，不必先改配置。
- **`日志 ▾` 菜单**：`打开日志文件`（资源管理器里定位到该文件；还没生成时打开数据目录）、
  `复制诊断信息`（版本、配置要点、服务组、端口、证书与自启状态）、`清空窗口日志`。
  原先露在动作区的「清空日志」收进这里，动作区因此精简为
  `待办 / 自检 / 服务组 / 打开配置 / 日志 / 退出`。
- **菜单统一实现**：新增通用 `popupMenu(hwnd, anchor, items)`（`menuBase=100`、
  `ClientToScreen` 锚点），服务组 / 待办 / 日志三个菜单共用——位置换算与关闭处理只有一份。

### 修复

- **托盘「退出」可能不生效**：窗口状态刷新原先与主流程抢同一个 `quit` 通道（容量 1），
  多一个接收者就可能让主流程永远等不到退出信号；改为独立的 `watchStop` 关闭信号。
- **缺证书不再只躺在日志里**：`--hidden` 启动时若反代因缺根证书起不来，
  现在会把这件事变成「待办」里的一条，点一下就能装好，而不是让用户自己去翻日志。

### 变更

- `log.file` 的语义：留空 / 未设置 = 写数据目录下的 `tongtu.log`；`off` / `none` / `-` = 不写文件
  （窗口与控制台不受影响）。老配置里为空串时会在校验阶段自动回填默认值，其它设置不动。

### 验证

- 真机：窗口 `自检` 跑完把结论写进卡片与状态条；`日志 ▾` 弹出 3 项、`待办` 弹出当前唯一一项
  （开机自启），按实际配置生成。
- 真机：`tongtu test` → 15 个域名：15 可用；日志文件正常写入并按 5 MB 轮转。
- 浅 / 深两套快照 PNG 复核布局（含 `待办 1`）。
- `gofmt` / `go vet` / `staticcheck` / `go test ./...` / `GOOS=linux go build ./...` 全绿；
  新增测试覆盖自检渲染与反馈、待办按钮的出现/位置、自检期间重复点击、清空日志菜单项、
  `log.file` 的路径与 `off` 语义、`Validate` 回填。

## [0.1.2] - 2026-10-21

### 修复

一次「异常退出会不会把网络搞坏」的专项审计，修掉 5 个在特定条件下会留下错误网络配置的问题：

- **状态文件可能被写成半截**（`internal/sysproxy/sysproxy_windows.go`）：`sysproxy.json` 是崩溃后
  还原原值的唯一依据，原来用"先截断再写"，断电 / 强杀正好撞上就会留下空文件或半截 JSON，
  原值从此找不回来。现在改为"临时文件 + 改名"（原子写）。
- **状态文件损坏后无路可走**：新增兜底——若系统代理仍指向一个**没人监听的本地端口**
  （崩溃遗留的典型形态），直接关掉代理、让系统回到直连，日志说明"原值无法恢复"；
  若端口上还有人在听（可能是用户自己的 Clash）或指向外部地址，则不猜、不动。
  新增 `TestDeadLocalProxy` / `TestStateRoundTripAndAtomic` / `TestReadStateCorrupt`。
- **接管只成功一半会留下半成品**：`Enable` 现在在写注册表失败时按原值回滚（回滚也失败才保留
  状态文件，留给退出时的还原逻辑）。
- **刚启动就崩溃没人还原**：监护进程改为在**接管之前**拉起（原来要等启动流程走完），
  消掉"注册表已经改了、监护还没起来"这段无人守护的窗口。
- **hosts 残留不再静默**：上一轮提权运行留下的托管块，如果这一轮没权限清理，启动摘要里直接显示
  「Hosts 残留：需以管理员身份运行一次」，而不是让相关域名悄悄连不上（新增 `hosts.Managed`）。
- **停用「无 SNI 反代」的顺序错误**：原来先关本地 `:443`/`:80` 监听、再改写 hosts，中间（以及改写
  失败时）hosts 仍指向 `127.0.0.1` 而没人监听，那些域名会连不上。现在先改写 hosts，失败则整体回滚、
  保持"反代仍开着"，并把原因显示在界面上。

### 验证

- 真机：`Stop-Process -Force` 强杀主进程 → 监护进程 **5 ms** 内把系统代理还原到原值，
  `sysproxy.json` / `guard.json` 均清理、无残留进程。
- 真机：造出"半截 JSON + 系统代理指向死端口"的现场 → `tongtu stop` 自动关闭代理并清理状态文件。
- 真机：`start`（运行中 `ProxyEnable=1`）→ `stop` → 完全回到启动前状态。
- `gofmt` / `go vet` / `staticcheck` / `go test ./...` / `GOOS=linux go build ./...` 全绿。

## [0.1.1] - 2026-10-19

### 修复

- **「服务组」菜单弹错位置**（`cmd/tongtu/ui_interact_windows.go`）：菜单锚点取的是几何层的
  **客户区**坐标，而 `TrackPopupMenu` 认的是**屏幕**坐标——少了一次换算，菜单就弹到窗口
  左上角附近而不是按钮下方。现在做 `ClientToScreen` 换算，并在弹出前把窗口设为前台窗口
  （否则菜单可能不跟随点击自动消失）。
- 新增 `TestServicesMenuPointIsScreenCoords`：断言菜单锚点换算回客户区正好是按钮左下角，
  且落在窗口的屏幕矩形内（把客户区坐标当屏幕坐标用会立刻被这条测出来）。

## [0.1.0] - 2026-10-19

本轮是「界面与交互」版本：窗口从「只能看」变成「能操作」，并补上悬停/过滤/轻量反馈这些
让日常使用顺手的东西。视觉语言（GDI 自绘、圆角卡片、冷灰 + 语义色）保持不变。

### 新增

- **窗口里直接操作**（`cmd/tongtu/ui_layout_windows.go`、`ui_interact_windows.go`、`ui_actions.go`）：
  状态条的三个开关胶囊可以直接点（`系统代理` / `Hosts` / `无SNI反代`），第二行右侧新增
  `服务组 ▾` / `打开配置` / `清空日志` / `退出` 四个动作按钮。`服务组` 菜单与托盘菜单共用同一套开关
  （`internal/tray/api.go` 导出复用入口，`internal/rules` 新增 `Name(id)`）。
- **悬停反馈与悬浮提示**：可点控件在常态下不铺底（幽灵控件），悬停/按下才浮出一层底色，
  光标变手型；同时用系统 `tooltips_class32` 给每个区域一句说明（过滤框是固定说明），
  提示条限制最大宽度、自动换行。命中判定与绘制共用同一份几何，不会出现「看着能点、点不到」。
- **轻量反馈**：切换成功后在状态条第二行显示「系统代理已开启」之类的短提示，约 3.2 秒后自动消失；
  失败则把原因显示在同一位置**并**写进日志——不弹模态框，不打断正在看的日志。
- **日志过滤**：卡片顶部新增过滤框（关键字不区分大小写、按整行匹配、续行跟随正文行，
  没有命中时给出提示行）与「只看警告」开关；输入 180ms 防抖，连续打字只重排一次。

### 变更

- **布局改为三层栅格**：第一行 = 小图标 + 标题 + 状态词（左）／三个胶囊（右，右对齐）；
  第二行 = 详情或轻量提示（左）／动作按钮（右）；卡片内 = 工具条（过滤框 + 只看警告）
  + 1 像素发丝线 + 日志区。右对齐控件从右往左排，增删按钮不会让左边内容跳动。
- **窗口最小尺寸 460×260 → 620×320**：状态条两行、三个胶囊与工具条都要放得下；
  更窄时按降级顺序先丢过滤框，最后才压缩标题（状态词与开关始终完整）。
- **父窗口改为双缓冲绘制**（内存 DC + BitBlt）：缩放窗口或状态刷新时不再看到
  「铺底色 → 画卡片 → 画状态条」的中间态。
- **交互底色改为「悬停才亮」**：常态不铺底色，悬停/按下才浮出（深色 16%、浅色 10% 与文字色混合），
  状态点因此始终落在窗口底色上，对比度不随交互状态变化。
- **exe 清单请求 comctl32 v6**（新增 `cmd/tongtu/tongtu.manifest`，重新生成 `rsrc_windows_amd64.syso`）：
  过滤框的空内容提示（`EM_SETCUEBANNER`）、提示条与滚动条才用现代外观——没有清单时
  v5 不支持该消息，过滤框会是一个没有说明的灰框。
- **开关切换不再阻塞窗口**：切换动作放到后台执行（hosts 接管要拉远程 hosts 源、无 SNI 反代要重新预热，
  慢的时候有好几秒），窗口线程先给一句「正在切换…」，做完再换成结果。切换期间再点开关不会发起
  第二次请求——状态还没回读，连着点只会得到与期望相反的结果（托盘菜单本来就是这个行为）。
- **日志追加不再在 `WM_SETREDRAW(FALSE)` 期间滚动**，并在追加后立刻 `UpdateWindow`：
  避免控件搬动屏幕像素却不重画被露出的那一条，导致旧文字与新文字短暂重叠。
- 服务组菜单、打开配置、清空日志、退出这些动作与托盘菜单走同一条代码路径，行为保持一致。

### 测试

- 新增 `ui_layout_windows_test.go`：几何与命中（胶囊右对齐不重叠、按钮右对齐、详情不压按钮、
  工具条/发丝线/日志区的上下关系、窄窗口丢过滤框、无状态项时的默认项）。
- 新增 `ui_filter_test.go`：过滤规则（大小写、续行跟随、只看警告保留警告+错误、空条件原样返回）。
- 新增 `ui_interact_windows_test.go`：把真实鼠标消息发给窗口过程，断言动作被正确触发
  （胶囊切换/失败提示/在别处松开不触发/空白处点击无动作/清空日志/只看警告/过滤防抖/toast 过期），
  并用一个可卡住的后台开关验证「切换中再点不重复发请求」。
- 新增 `window_stale_windows_test.go`：比较日志控件自己 DC 上的墨量与重新渲染的墨量，
  防止滚动搬像素留下的残影（`WM_PRINTCLIENT` 渲染看不出来的那类问题）。
- `ui_snapshot_windows_test.go`：设 `TONGTU_UI_SNAPSHOT=<目录>` 时导出深浅色与悬停/提示共四张 PNG
  （连子控件一起画），做视觉自查。
- `palette_windows_test.go` 扩展：正文对悬停底色 ≥7:1、状态点 ≥3:1、过滤框文字 ≥7:1 / ≥4.5:1，
  并把悬停/输入底色纳入「冷灰同色相」检查。

### 验证记录（本机实测）

- 非管理员启动：hosts 降级为「未生效（需要管理员身份运行）」，状态词显示「部分生效」，符合预期。
- 点「无SNI反代」胶囊，`tongtu status` 依次显示 `关 → 开（仅系统代理路径）→ 关`，来回切换正常。
- 悬停胶囊与过滤框：浮出底色、光标变手型、提示条正常弹出（截图存档）。
- `tongtu stop` 后进程正常退出，`ProxyEnable` 回到启动前的 0，hosts 未被改动。


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
