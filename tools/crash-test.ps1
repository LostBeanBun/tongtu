# crash-test.ps1 — 崩溃防护验收：强杀通途主进程，检查系统状态是否被自动还原。
#
# 为什么要跑这个：
#   通途接管了系统代理 / hosts 之后，一旦异常退出（崩溃、任务管理器强杀、进程树被连带结束），
#   系统就会留着「指向已经不存在的本地代理」的设置，表现为整个浏览器都上不了网。
#   兜底靠的是独立进程 `tongtu __guard`：它盯着主进程，发现主进程死了就立刻还原。
#
#   这个脚本模拟最恶劣的情况——直接 taskkill /F，不给任何清理机会——然后验证：
#     1. 系统代理是否还原成启动之前的样子；
#     2. hosts 里是否没有残留托管块；
#     3. 监护进程是否自己也退干净了（不留僵尸进程）；
#     4. 数据目录里是否没有残留的 guard.json。
#
# 用法：
#   .\tools\crash-test.ps1                        # 用 dist\tongtu.exe
#   .\tools\crash-test.ps1 -RestartAfter          # 测完把通途重新拉起来
#   .\tools\crash-test.ps1 -Exe .\dist\tongtu.exe -WaitRestoreSec 90
#
# 注意：
#   - 脚本会快照并在结束时还原系统代理与 hosts，不会破坏你原本的网络配置。
#   - 非管理员运行时 hosts 本来就接管不了（属预期），此时只验证系统代理那一半。
#   - 如果本脚本是被「作业对象」包着的终端启动的（某些 IDE / CI / 任务计划里会这样），
#     监护进程可能无法脱离主进程，异常退出时会一起被带走——这是启动方式的限制，
#     不是代码问题；输出里会区分这两种结论，并可查看 guard.log 佐证。
param(
    [string]$Exe = "",
    [int]$WaitTakeoverSec = 40,   # 等「已接管」的最长时间
    [int]$WaitRestoreSec = 60,    # 等「已还原」的最长时间
    [switch]$SkipRestore,         # 不还原快照（默认会还原）
    [switch]$RestartAfter         # 测完重新启动一个实例（若跑之前本来就在运行）
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
if (-not $Exe) { $Exe = Join-Path $root "dist\tongtu.exe" }
if (-not (Test-Path $Exe)) { throw "找不到可执行文件: $Exe（先执行 .\build.ps1）" }
$Exe = (Resolve-Path $Exe).Path

$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings"
$hostsPath = Join-Path $env:WINDIR "System32\drivers\etc\hosts"
$dataDir = Join-Path $env:APPDATA "tongtu"
$cfgPath = Join-Path $dataDir "config.yaml"
$guardState = Join-Path $dataDir "guard.json"
$guardLog = Join-Path $dataDir "guard.log"
$beginMark = "# >>> tongtu managed block >>>"

# 监听地址（用来识别「指向通途但通途并不在跑」的自代理残留）
$listen = "127.0.0.1:19180"
$control = "127.0.0.1:19181"
if (Test-Path $cfgPath) {
    $cfgText = Get-Content -Path $cfgPath -Raw
    if ($cfgText -match "(?m)^\s*listen:\s*(\S+)") { $listen = $Matches[1].Trim('"', "'") }
    if ($cfgText -match "(?m)^\s*control:\s*(\S+)") { $control = $Matches[1].Trim('"', "'") }
}

function Get-ProxySnapshot {
    $k = Get-ItemProperty -Path $regPath -ErrorAction SilentlyContinue
    [pscustomobject]@{
        Enable     = if ($null -ne $k.ProxyEnable) { [int]$k.ProxyEnable } else { 0 }
        Server     = [string]$k.ProxyServer
        AutoConfig = [string]$k.AutoConfigURL
    }
}

function Set-ProxySnapshot($snap) {
    New-ItemProperty -Path $regPath -Name "ProxyEnable" -Value $snap.Enable -PropertyType DWord -Force | Out-Null
    if ($snap.Server) {
        New-ItemProperty -Path $regPath -Name "ProxyServer" -Value $snap.Server -PropertyType String -Force | Out-Null
    } else {
        Remove-ItemProperty -Path $regPath -Name "ProxyServer" -ErrorAction SilentlyContinue
    }
    if ($snap.AutoConfig) {
        New-ItemProperty -Path $regPath -Name "AutoConfigURL" -Value $snap.AutoConfig -PropertyType String -Force | Out-Null
    } else {
        Remove-ItemProperty -Path $regPath -Name "AutoConfigURL" -ErrorAction SilentlyContinue
    }
}

function Get-TongtuProcesses { @(Get-Process -Name tongtu -ErrorAction SilentlyContinue) }

function Test-HostsHasManagedBlock {
    if (-not (Test-Path $hostsPath)) { return $false }
    $lines = Get-Content -Path $hostsPath -ErrorAction SilentlyContinue
    return [bool]($lines | Where-Object { $_ -like "*$beginMark*" })
}

function Test-PortOpen([string]$addr) {
    $parts = $addr.Split(":")
    $c = New-Object System.Net.Sockets.TcpClient
    try { return $c.ConnectAsync($parts[0], [int]$parts[1]).Wait(600) } catch { return $false } finally { $c.Close() }
}

Write-Host "通途崩溃防护验收" -ForegroundColor Cyan
Write-Host "  exe:  $Exe"
Write-Host "  监听: $listen（控制 $control）"
Write-Host ""

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)

# —— 1. 让已有实例正常退出 ——
$wasRunning = (Get-TongtuProcesses).Count -gt 0
if ($wasRunning) {
    $running = Get-TongtuProcesses
    Write-Host "[1/6] 检测到已有实例（pid $($running.Id -join ', ')），先让它正常退出…" -ForegroundColor Yellow
    & $Exe stop 2>&1 | Out-Null
    Start-Sleep -Seconds 3
    Get-TongtuProcesses | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Seconds 1
} else {
    Write-Host "[1/6] 没有正在运行的通途实例。" -ForegroundColor Gray
}

# —— 2. 清掉「无人认领的自代理残留」 ——
# 上一次崩溃（或上一次测试）可能留下 ProxyEnable=1 + ProxyServer=本工具端口，而通途并没在跑。
# 不清掉的话，下面「等待接管」会瞬间通过，整个测试就没有意义了。
$stale = Get-ProxySnapshot
$staleLeftover = ($stale.Enable -eq 1) -and ($stale.Server -eq $listen) -and ((Get-TongtuProcesses).Count -eq 0)
if ($staleLeftover) {
    Write-Host "[2/6] 发现自代理残留（ProxyEnable=1 指向 $listen，但没有实例在跑）——先清干净。" -ForegroundColor Yellow
    New-ItemProperty -Path $regPath -Name "ProxyEnable" -Value 0 -PropertyType DWord -Force | Out-Null
    Write-Host "      这也正说明「异常退出后没兜底还原」的后果：浏览器会全部上不了网。" -ForegroundColor DarkYellow
} else {
    Write-Host "[2/6] 没有自代理残留。" -ForegroundColor Gray
}

# —— 3. 快照「启动前」的干净基线 ——
$snapProxy = Get-ProxySnapshot
$snapHosts = if (Test-Path $hostsPath) { [IO.File]::ReadAllBytes($hostsPath) } else { $null }
Write-Host ("[3/6] 基线: ProxyEnable={0} ProxyServer='{1}'；hosts 托管块={2}；管理员={3}" -f `
    $snapProxy.Enable, $snapProxy.Server, (Test-HostsHasManagedBlock), $admin)

$failures = New-Object System.Collections.Generic.List[string]
$notes = New-Object System.Collections.Generic.List[string]

# —— 4. 启动并等真正就绪 ——
# 「就绪」= 控制端口在听 + 系统代理已被指向本地 + 监护进程已经起来。
# 只看 ProxyEnable 是不够的：残留值会让它立刻为 1，于是可能在主进程还没起稳时就把它杀了。
Write-Host "[4/6] 启动并等待就绪…" -ForegroundColor Gray
$proc = Start-Process -FilePath $Exe -PassThru
$mainPid = $proc.Id
$sw = [Diagnostics.Stopwatch]::StartNew()
$guardPids = @()
$ready = $false
$tookOver = $false
while ($sw.Elapsed.TotalSeconds -lt $WaitTakeoverSec) {
    $now = Get-ProxySnapshot
    if ($now.Enable -eq 1) { $tookOver = $true }
    $procs = Get-TongtuProcesses
    $guardPids = @($procs | Where-Object { $_.Id -ne $mainPid } | Select-Object -ExpandProperty Id)
    if ($tookOver -and (Test-PortOpen $control) -and $guardPids.Count -gt 0) { $ready = $true; break }
    if ($proc.HasExited) { break }
    Start-Sleep -Milliseconds 300
}
$readySec = [math]::Round($sw.Elapsed.TotalSeconds, 1)
Write-Host ("      主进程 pid={0}；就绪用时 {1}s；系统代理已接管={2}；监护进程={3}" -f `
    $mainPid, $readySec, $tookOver, $(if ($guardPids.Count) { "pid $($guardPids -join ', ')" } else { "无" }))

if ($proc.HasExited) {
    Write-Host "结论：无法测试。主进程启动后就退出了（先单独跑一次 $Exe status 看原因）。" -ForegroundColor Red
    $proc.WaitForExit()
    exit 2
}
if (-not $tookOver) {
    Write-Host "结论：无法测试。本次启动没有接管系统代理（配置里 system_proxy: false？），崩溃防护无从验证。" -ForegroundColor Yellow
    & taskkill.exe /F /T /PID $mainPid 2>&1 | Out-Null
    exit 2
}
if ($guardPids.Count -eq 0) {
    $failures.Add("托管了系统代理却没有监护进程——异常退出时无人兜底")
}

# —— 5. 强杀并等还原 ——
Write-Host "[5/6] 强杀主进程（taskkill /F，不给任何清理机会）…" -ForegroundColor Gray
& taskkill.exe /F /PID $mainPid 2>&1 | Out-Null
Start-Sleep -Seconds 2
$killOk = $proc.HasExited
if (-not $killOk) {
    & taskkill.exe /F /T /PID $mainPid 2>&1 | Out-Null
    $killOk = $proc.HasExited
    $notes.Add("单杀主进程没杀掉，退而用 /T 连子进程一起杀（说明本机启动方式让监护进程无法脱离）")
}

$restored = $false
$sw.Restart()
while ($sw.Elapsed.TotalSeconds -lt $WaitRestoreSec) {
    $now = Get-ProxySnapshot
    $procs = Get-TongtuProcesses
    if (($now.Enable -eq $snapProxy.Enable) -and
        ($now.Server -eq $snapProxy.Server) -and
        (-not (Test-Path $guardState)) -and
        ($procs.Count -eq 0) -and
        (-not (Test-HostsHasManagedBlock))) { $restored = $true; break }
    Start-Sleep -Milliseconds 500
}
$restoreSec = [math]::Round($sw.Elapsed.TotalSeconds, 1)

# —— 6. 断言与报告 ——
$now = Get-ProxySnapshot
$procs = Get-TongtuProcesses
$hostsBlock = Test-HostsHasManagedBlock
$guardLeft = Test-Path $guardState

if (-not $killOk) { $failures.Add("主进程没被杀掉，测试无效") }
if (($now.Enable -ne $snapProxy.Enable) -or ($now.Server -ne $snapProxy.Server)) {
    $failures.Add(("系统代理没还原: 现在 ProxyEnable={0} ProxyServer='{1}'，应为 ProxyEnable={2} ProxyServer='{3}'" -f `
        $now.Enable, $now.Server, $snapProxy.Enable, $snapProxy.Server))
}
if ($hostsBlock) { $failures.Add("hosts 里残留托管块（$beginMark）") }
if ($guardLeft) { $failures.Add("数据目录残留 guard.json") }
if ($procs.Count -gt 0) { $failures.Add("还有通途进程没退出: pid $($procs.Id -join ', ')") }

Write-Host "[6/6] 结果" -ForegroundColor Gray
Write-Host ""
Write-Host "  检查项                       期望                        实际" -ForegroundColor Cyan
$rows = @(
    @{ Name = "系统代理已还原";   Want = "ProxyEnable=$($snapProxy.Enable)"; Got = "ProxyEnable=$($now.Enable)"; Ok = (($now.Enable -eq $snapProxy.Enable) -and ($now.Server -eq $snapProxy.Server)) },
    @{ Name = "hosts 无托管块";   Want = "无";                             Got = $(if ($hostsBlock) { "有" } else { "无" }); Ok = (-not $hostsBlock) },
    @{ Name = "无 guard.json 残留"; Want = "无";                           Got = $(if ($guardLeft) { "有" } else { "无" }); Ok = (-not $guardLeft) },
    @{ Name = "无通途进程残留";   Want = "0 个";                           Got = "$($procs.Count) 个"; Ok = ($procs.Count -eq 0) }
)
foreach ($r in $rows) {
    $mark = if ($r.Ok) { "OK  " } else { "FAIL" }
    $color = if ($r.Ok) { "Green" } else { "Red" }
    Write-Host ("  [{0}] {1,-24} {2,-27} {3}" -f $mark, $r.Name, $r.Want, $r.Got) -ForegroundColor $color
}
Write-Host ""

# guard.log 取证：区分「环境限制」与「代码问题」
$guardHint = ""
if (Test-Path $guardLog) {
    $tail = Get-Content -Path $guardLog -Tail 40 -ErrorAction SilentlyContinue
    if ($tail | Where-Object { $_ -match "作业对象|脱离|breakaway" }) { $guardHint = "guard.log 提到作业对象/脱离失败" }
    Write-Host "guard.log 末 10 行：" -ForegroundColor Gray
    $tail | Select-Object -Last 10 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
    Write-Host ""
}

if ($restored -and $failures.Count -eq 0) {
    Write-Host "结论：通过。主进程被强杀后 $restoreSec 秒内系统状态已由监护进程还原。" -ForegroundColor Green
} elseif ($guardHint) {
    Write-Host "结论：环境限制。监护进程无法脱离当前启动方式（$guardHint），" -ForegroundColor Yellow
    Write-Host "      被连带结束所以没能兜底还原。请从资源管理器双击或独立终端再跑一次；" -ForegroundColor Yellow
    Write-Host "      启动摘要里也会显示「崩溃防护：受限」提醒这种情况。" -ForegroundColor Yellow
    foreach ($f in $failures) { Write-Host "      - $f" -ForegroundColor Yellow }
} else {
    Write-Host "结论：未通过。监护进程没能还原系统状态，疑似代码问题。" -ForegroundColor Red
    foreach ($f in $failures) { Write-Host "      - $f" -ForegroundColor Red }
}
foreach ($n in $notes) { Write-Host "备注：$n" -ForegroundColor DarkYellow }
Write-Host ""

# —— 收尾：还原基线 ——
if (-not $SkipRestore) {
    $cur = Get-ProxySnapshot
    if (($cur.Enable -ne $snapProxy.Enable) -or ($cur.Server -ne $snapProxy.Server) -or ($cur.AutoConfig -ne $snapProxy.AutoConfig)) {
        Set-ProxySnapshot $snapProxy
        Write-Host "已把系统代理还原为启动之前的状态。" -ForegroundColor Gray
    }
    if ($snapHosts -and (Test-HostsHasManagedBlock)) {
        try {
            [IO.File]::WriteAllBytes($hostsPath, $snapHosts)
            Write-Host "已把 hosts 还原为启动之前的内容。" -ForegroundColor Gray
        } catch {
            Write-Host "hosts 还原失败（需要管理员权限）：$($_.Exception.Message)" -ForegroundColor Yellow
        }
    }
}

if ($RestartAfter -and $wasRunning) {
    Start-Process -FilePath $Exe | Out-Null
    Write-Host "已重新启动通途（跑之前它本来就在运行）。" -ForegroundColor Gray
}

if ($failures.Count -gt 0) { exit 1 }
exit 0
