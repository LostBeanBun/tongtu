# ui-smoke-intent.ps1 — 方向 C 界面对外口径的真机冒烟（非交互，配合 ui-smoke.ps1 使用）。
#
# 为什么单独一个脚本：
#   方向 C 改的是「状态/待办怎么对外解释」——意图与实际持有要分开、被顶掉要归因实话、
#   --no-sysproxy 不能被误报成「该开却没开」。这些都能从 /status 与 /ui/layout 上读到确定结果，
#   不需要挪鼠标；ui-smoke.ps1 负责「点得到、点得准」，这里负责「说的对不对」。
#
# 检查项（6 项）：
#   1. 默认启动：want=true、实际持有=true、无归因备注
#   2. 系统代理被别的程序顶掉：实际=false、备注「已被其他程序接管」、CLI status 同口径
#   3. 被顶掉后待办出现「重新接管系统代理」
#   4. 把代理改回通途：实际=true、备注清空、待办回到基线
#   5. `--no-sysproxy` 启动：want=false、实际=false、无归因备注、待办不增加（不误报「重新接管」）
#   6. 收尾干净：进程 / 注册表 / RunOnce / guard.json 全部复原
#
# 用法：
#   .\tools\ui-smoke-intent.ps1            # 用 dist\tongtu.exe
#   .\tools\ui-smoke-intent.ps1 -Exe .\dist\tongtu.exe
#
# 注意：
#   - 会临时改写本机的系统代理（HKCU），结束时按快照还原。
#   - 不需要管理员权限，不移动鼠标、不抢前台。
param(
    [string]$Exe = "",
    [int]$WaitSec = 40
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
if (-not $Exe) { $Exe = Join-Path $root "dist\tongtu.exe" }
if (-not (Test-Path $Exe)) { throw "找不到可执行文件: $Exe（先执行 .\build.ps1）" }
$Exe = (Resolve-Path $Exe).Path

$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings"
$runOnceKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\RunOnce"
$runOnceName = "TongtuRecover"
$dataDir = Join-Path $env:APPDATA "tongtu"
$cfgPath = Join-Path $dataDir "config.yaml"
$guardPath = Join-Path $dataDir "guard.json"
$statePath = Join-Path $dataDir "sysproxy.json"

if (-not (Test-Path $cfgPath)) { throw "找不到配置文件: $cfgPath（先手动启动一次通途）" }
$cfgText = Get-Content -Path $cfgPath -Raw
$listen = "127.0.0.1:19180"
$control = "127.0.0.1:19181"
if ($cfgText -match "(?m)^\s*listen:\s*(\S+)") { $listen = $Matches[1].Trim('"', "'") }
if ($cfgText -match "(?m)^\s*control:\s*(\S+)") { $control = $Matches[1].Trim('"', "'") }

# 不走系统代理：检查过程中 ProxyEnable 会被改成 1（指向通途 / 假地址），
# 默认 HttpClient 会把「查自己」的请求也塞进代理。
$handler = [System.Net.Http.SocketsHttpHandler]::new()
$handler.UseProxy = $false
$script:http = [System.Net.Http.HttpClient]::new($handler)
$script:http.Timeout = [TimeSpan]::FromSeconds(3)

$script:failures = New-Object System.Collections.Generic.List[string]
$script:notes = New-Object System.Collections.Generic.List[string]

# 汇总行的初值：中途抛错时也不会读到未定义变量。
$ok1 = $ok2 = $cliOk = $ok3 = $ok4 = $ok4b = $ok5 = $clean = $false
$todoBase = $todoStolen = 0
$st2 = [pscustomobject]@{ proxy_note = "" }

function Get-Status {
    try {
        $json = $script:http.GetStringAsync("http://$control/status").GetAwaiter().GetResult()
        return $json | ConvertFrom-Json
    } catch { return $null }
}

function Get-UILayout {
    try {
        $json = $script:http.GetStringAsync("http://$control/ui/layout").GetAwaiter().GetResult()
        return $json | ConvertFrom-Json
    } catch { return $null }
}

# 待办数量取自 /ui/layout 里「待办 N」按钮（没有待办时按钮不存在）。
function Get-TodoCount {
    $l = Get-UILayout
    if ($null -eq $l) { return -1 }
    $r = @($l.regions | Where-Object { $_.id -eq "todo" }) | Select-Object -First 1
    if ($null -eq $r) { return 0 }
    if ([string]$r.label -match '(\d+)') { return [int]$Matches[1] }
    return 0
}

# 待办数量的基线必须等「布局已就绪 + 连续两次读数一致」才可信：
# 刚启动时窗口还没画过，/ui/layout 可能根本没有 todo 区域，读出来是 0——
# 直接拿它当基线会让后面的「回到基线」永远判不中（本脚本第一版就踩了）。
function Wait-TodoStable([int]$sec) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $prev = $null
    while ($sw.Elapsed.TotalSeconds -lt $sec) {
        $n = Get-TodoCount
        if ($n -ge 0 -and ($null -ne $prev) -and ($n -eq $prev)) { return $n }
        if ($n -ge 0) { $prev = $n }
        Start-Sleep -Seconds 3
    }
    if ($null -eq $prev) { return -1 }
    return $prev
}

function Get-ProxyEnable { try { [int](Get-ItemProperty $regPath -ErrorAction Stop).ProxyEnable } catch { -1 } }
function Get-ProxyServer { try { [string](Get-ItemProperty $regPath -ErrorAction Stop).ProxyServer } catch { "" } }
function Get-RunOnceValue {
    $k = Get-ItemProperty -Path $runOnceKey -Name $runOnceName -ErrorAction SilentlyContinue
    if ($null -eq $k) { return "" }
    return [string]$k.$runOnceName
}
function Get-TongtuProcesses { @(Get-Process -Name tongtu -ErrorAction SilentlyContinue) }

function Wait-Until([scriptblock]$cond, [int]$sec) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $sec) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

function Wait-Status([scriptblock]$cond, [int]$sec) {
    # 注意：与 ui-smoke.ps1 一致——带参数的判定脚本块由这里负责传入状态。
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $sec) {
        $st = Get-Status
        if ($null -ne $st -and (& $cond $st)) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

function Stop-Tongtu {
    Start-Process -FilePath $Exe -ArgumentList 'stop' -Wait | Out-Null
    if (-not (Wait-Until { (Get-TongtuProcesses).Count -eq 0 } 20)) {
        Get-TongtuProcesses | Stop-Process -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 1
        $script:notes.Add("实例没能在 20 秒内正常退出，已强制结束")
    }
}

function Start-Tongtu([string[]]$extra) {
    $startArgs = @('start', '--no-mitm') + $extra
    Start-Process -FilePath $Exe -ArgumentList $startArgs | Out-Null
    $ok = Wait-Status { param($s) $s.running } $WaitSec
    if (-not $ok) { throw "实例未在 $WaitSec 秒内就绪" }
}

function Set-SystemProxy([int]$enable, [string]$server) {
    New-ItemProperty -Path $regPath -Name "ProxyEnable" -Value $enable -PropertyType DWord -Force | Out-Null
    New-ItemProperty -Path $regPath -Name "ProxyServer" -Value $server -PropertyType String -Force | Out-Null
}

Write-Host "通途方向 C 口径冒烟（非交互）" -ForegroundColor Cyan
Write-Host "  exe:      $Exe"
Write-Host "  控制端口: $control"
Write-Host ""

# —— 0. 收起点：确保没有实例 / 遗留状态 ——
if ((Get-TongtuProcesses).Count -gt 0) {
    Write-Host "[0] 检测到已有实例，先正常退出…" -ForegroundColor Gray
    Stop-Tongtu
}
$snapEnable = Get-ProxyEnable
$snapServer = Get-ProxyServer
Write-Host "[0] 基线: ProxyEnable=$snapEnable ProxyServer=$snapServer RunOnce='$(Get-RunOnceValue)'" -ForegroundColor Gray

try {
    # —— 1. 默认启动：意图 = 实际 = true，且没有归因备注 ——
    Write-Host "[1] 默认启动，检查意图口径…" -ForegroundColor Gray
    Start-Tongtu @()
    $st = Get-Status
    $ok1 = ($st.want_system_proxy -eq $true) -and ($st.system_proxy -eq $true) -and ([string]$st.proxy_note -eq "")
    if (-not $ok1) {
        $script:failures.Add("默认启动意图口径不对：want=$($st.want_system_proxy) have=$($st.system_proxy) note='$($st.proxy_note)'")
    }
    $todoBase = Wait-TodoStable 40
    if ($todoBase -lt 0) { throw "等不到稳定的待办基线（/ui/layout 不可用？）" }
    Write-Host "      want=$($st.want_system_proxy) have=$($st.system_proxy) note='$($st.proxy_note)' 待办=$todoBase" -ForegroundColor Gray

    # —— 2. 系统代理被顶掉：实际=false、归因实话、CLI 同口径 ——
    Write-Host "[2] 模拟被别的程序顶掉系统代理…" -ForegroundColor Gray
    Set-SystemProxy 1 "127.0.0.1:9"
    $ok2 = Wait-Status { param($s) ($s.system_proxy -eq $false) -and ([string]$s.proxy_note -eq "已被其他程序接管") } 10
    $st2 = Get-Status
    if (-not $ok2) {
        $script:failures.Add("被顶掉后归因不对：have=$($st2.system_proxy) note='$($st2.proxy_note)'（期望 false / 已被其他程序接管）")
    }
    $cli = (& $Exe status 2>&1 | Out-String)
    $cliOk = $cli -match "系统代理: 关（已被其他程序接管）"
    if (-not $cliOk) {
        $script:failures.Add("CLI status 没有按同一口径归因（输出未出现「系统代理: 关（已被其他程序接管）」）")
    }
    Write-Host "      have=$($st2.system_proxy) note='$($st2.proxy_note)' CLI同口径=$cliOk" -ForegroundColor Gray

    # —— 3. 待办出现「重新接管系统代理」（30 秒兜底刷新，等 40 秒） ——
    Write-Host "[3] 等待待办刷新…" -ForegroundColor Gray
    $ok3 = Wait-Until { (Get-TodoCount) -ge ($todoBase + 1) } 40
    $todoStolen = Get-TodoCount
    if (-not $ok3) {
        $script:failures.Add("被顶掉后没有出现「重新接管」待办：基线=$todoBase 现在=$todoStolen")
    }
    Write-Host "      待办 $todoBase -> $todoStolen" -ForegroundColor Gray

    # —— 4. 改回通途：持有恢复、备注清空、待办回到基线 ——
    Write-Host "[4] 把系统代理改回通途…" -ForegroundColor Gray
    Set-SystemProxy 1 $listen
    $ok4 = Wait-Status { param($s) ($s.system_proxy -eq $true) -and ([string]$s.proxy_note -eq "") } 10
    if (-not $ok4) {
        $st4 = Get-Status
        $script:failures.Add("改回通途后状态没恢复：have=$($st4.system_proxy) note='$($st4.proxy_note)'")
    }
    $ok4b = Wait-Until { (Get-TodoCount) -eq $todoBase } 40
    if (-not $ok4b) {
        $script:failures.Add("改回通途后待办没有回到基线：期望 $todoBase，现在 $(Get-TodoCount)")
    }
    Write-Host "      恢复=$ok4 待办回落=$ok4b（$(Get-TodoCount)）" -ForegroundColor Gray

    # —— 5. --no-sysproxy：意图=false，不误报「重新接管」待办 ——
    Write-Host "[5] 用 --no-sysproxy 重启，检查意图口径…" -ForegroundColor Gray
    Stop-Tongtu
    Start-Tongtu @('--no-sysproxy')
    $st5 = Get-Status
    $ok5 = ($st5.want_system_proxy -eq $false) -and ($st5.system_proxy -eq $false) -and ([string]$st5.proxy_note -eq "")
    if (-not $ok5) {
        $script:failures.Add("--no-sysproxy 意图口径不对：want=$($st5.want_system_proxy) have=$($st5.system_proxy) note='$($st5.proxy_note)'")
    }
    $todoNoSys = Wait-TodoStable 40
    if ($todoNoSys -ne $todoBase) {
        $script:failures.Add("--no-sysproxy 下待办数变了：基线=$todoBase 现在=$todoNoSys（不该误报「重新接管」）")
    }
    $enableNoSys = Get-ProxyEnable
    if ($snapEnable -ne $enableNoSys) {
        $script:failures.Add("--no-sysproxy 下系统代理被改动：ProxyEnable=$enableNoSys，基线=$snapEnable")
    }
    Write-Host "      want=$($st5.want_system_proxy) have=$($st5.system_proxy) note='$($st5.proxy_note)' 待办=$todoNoSys ProxyEnable=$enableNoSys" -ForegroundColor Gray

    # —— 6. 收尾干净 ——
    Write-Host "[6] 收尾…" -ForegroundColor Gray
    Stop-Tongtu
    $clean = ((Get-TongtuProcesses).Count -eq 0) -and ((Get-ProxyEnable) -eq $snapEnable) -and
        ((Get-ProxyServer) -eq $snapServer) -and ((Get-RunOnceValue) -eq "") -and
        (-not (Test-Path $guardPath)) -and (-not (Test-Path $statePath))
    if (-not $clean) {
        $script:failures.Add("收尾不干净：进程=$((Get-TongtuProcesses).Count) ProxyEnable=$(Get-ProxyEnable)/$snapEnable ProxyServer='$(Get-ProxyServer)'/'$snapServer' RunOnce='$(Get-RunOnceValue)' guard.json=$(Test-Path $guardPath) sysproxy.json=$(Test-Path $statePath)")
    }
    Write-Host "      进程=$((Get-TongtuProcesses).Count) ProxyEnable=$(Get-ProxyEnable) RunOnce='$(Get-RunOnceValue)'" -ForegroundColor Gray
} finally {
    # 兜底：停实例、按快照还原系统代理与 RunOnce。
    if ((Get-TongtuProcesses).Count -gt 0) { Stop-Tongtu }
    if ((Get-ProxyEnable) -ne $snapEnable -or (Get-ProxyServer) -ne $snapServer) {
        Set-SystemProxy $snapEnable $snapServer
        $script:notes.Add("按快照还原了系统代理")
    }
    if ((Get-RunOnceValue) -ne "") {
        Remove-ItemProperty -Path $runOnceKey -Name $runOnceName -ErrorAction SilentlyContinue
        $script:notes.Add("收尾时发现 RunOnce 项还在，已手动清除")
    }
}

Write-Host ""
$rows = @(
    @{ Name = "默认意图口径"; Ok = $ok1; Detail = "want/have/note 一致" },
    @{ Name = "被顶掉归因";   Ok = $ok2; Detail = "note='$($st2.proxy_note)'" },
    @{ Name = "CLI 同口径";   Ok = $cliOk; Detail = "status 文案含归因" },
    @{ Name = "待办出现";     Ok = $ok3; Detail = "$todoBase -> $todoStolen" },
    @{ Name = "恢复回落";     Ok = ($ok4 -and $ok4b); Detail = "have/note/待办都恢复" },
    @{ Name = "--no-sysproxy"; Ok = $ok5; Detail = "不误报、不改动系统代理" },
    @{ Name = "收尾干净";     Ok = $clean; Detail = "进程 / 注册表 / RunOnce / 状态文件" }
)
foreach ($r in $rows) {
    $mark = if ($r.Ok) { "OK  " } else { "FAIL" }
    $color = if ($r.Ok) { "Green" } else { "Red" }
    Write-Host ("  [{0}] {1,-12} {2}" -f $mark, $r.Name, $r.Detail) -ForegroundColor $color
}
Write-Host ""
foreach ($n in $script:notes) { Write-Host "备注：$n" -ForegroundColor DarkYellow }

if ($script:failures.Count -eq 0) {
    Write-Host "结论：通过。意图口径、归因、待办与收尾都符合预期。" -ForegroundColor Green
    exit 0
}
Write-Host "结论：未通过。" -ForegroundColor Red
foreach ($f in $script:failures) { Write-Host "      - $f" -ForegroundColor Red }
exit 1
