# recover-test.ps1 — 断电自愈验收：模拟「掉电 / 蓝屏」之后，登录时的 __recover 能不能把网络还原干净。
#
# 为什么要跑这个：
#   崩溃 / 强杀有监护进程（__guard）兜底；但掉电、蓝屏、长按电源键时没有任何代码有机会执行。
#   机器再打开时，系统代理还指着 127.0.0.1:19180、hosts 里还写着托管块，而通途并没有在跑——
#   表现就是「开机后网页全打不开」。兜底是接管期间登记在
#   HKCU\Software\Microsoft\Windows\CurrentVersion\RunOnce 里的那条命令：
#
#       "<exe>" __recover
#
#   登录时由 Windows 执行一次，还原系统代理与 hosts，然后把自己清掉。
#
#   这个脚本按最恶劣的顺序模拟断电：
#     1. 启动通途，等它真正接管（并确认 RunOnce 项已登记）；
#     2. 先杀监护进程、再杀主进程——两份还原逻辑都来不及跑，留下「脏状态」；
#     3. 像登录时那样执行一次 `tongtu __recover`；
#     4. 断言系统代理恢复成启动前的样子、RunOnce 项被清除、guard.json 被回收；
#     5. 再跑一次 __recover，确认幂等（没有残留状态时什么都不做）。
#
# 用法：
#   .\tools\recover-test.ps1                       # 用 dist\tongtu.exe
#   .\tools\recover-test.ps1 -Exe .\dist\tongtu.exe -WaitSec 40
#
# 注意：
#   - 脚本会快照并在结束时还原系统代理与 hosts，不会破坏你原本的网络配置。
#   - 用 --no-hosts --no-mitm 启动：本项只验证系统代理这条路径（hosts 清理需要管理员权限，
#     RunOnce 又是以当前用户权限运行的，那是另一条路径，见脚本末尾的说明）。
#   - 跑之前若有实例在运行会先让它正常退出；退不掉就中止，不会强杀别人的会话。
param(
    [string]$Exe = "",
    [int]$WaitSec = 40,           # 等「接管 + RunOnce 登记」的最长时间
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
$runOnceKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\RunOnce"
$runOnceName = "TongtuRecover"
$runOnceWant = '"{0}" __recover' -f $Exe
$hostsPath = Join-Path $env:WINDIR "System32\drivers\etc\hosts"
$dataDir = Join-Path $env:APPDATA "tongtu"
$cfgPath = Join-Path $dataDir "config.yaml"
$guardState = Join-Path $dataDir "guard.json"
$logFile = Join-Path $dataDir "tongtu.log"
$beginMark = "# >>> tongtu managed block >>>"

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

function Get-RunOnceValue {
    $k = Get-ItemProperty -Path $runOnceKey -Name $runOnceName -ErrorAction SilentlyContinue
    if ($null -eq $k) { return "" }
    return [string]$k.$runOnceName
}

function Test-PortOpen([string]$addr) {
    $parts = $addr.Split(":")
    $c = New-Object System.Net.Sockets.TcpClient
    try { return $c.ConnectAsync($parts[0], [int]$parts[1]).Wait(600) } catch { return $false } finally { $c.Close() }
}

function Test-HostsHasManagedBlock {
    if (-not (Test-Path $hostsPath)) { return $false }
    $lines = Get-Content -Path $hostsPath -ErrorAction SilentlyContinue
    return [bool]($lines | Where-Object { $_ -like "*$beginMark*" })
}

Write-Host "通途断电自愈验收" -ForegroundColor Cyan
Write-Host "  exe:     $Exe"
Write-Host "  RunOnce: $runOnceWant"
Write-Host "  监听:    $listen（控制 $control）"
Write-Host ""

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)

# —— 1. 让已有实例正常退出（不强杀别人的会话）——
$wasRunning = (Get-TongtuProcesses).Count -gt 0
if ($wasRunning) {
    $running = Get-TongtuProcesses
    Write-Host "[1/7] 检测到已有实例（pid $($running.Id -join ', ')），先让它正常退出…" -ForegroundColor Yellow
    & $Exe stop 2>&1 | Out-Null
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-TongtuProcesses).Count -gt 0 -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 300 }
    if ((Get-TongtuProcesses).Count -gt 0) {
        Write-Host "结论：无法测试。已有实例没有正常退出（pid $((Get-TongtuProcesses).Id -join ', ')）。" -ForegroundColor Red
        exit 2
    }
} else {
    Write-Host "[1/7] 没有正在运行的通途实例。" -ForegroundColor Gray
}

# —— 2. 快照基线 + 清掉上一次的残留 ——
# 上一轮测试（或真的掉过电）可能留下 ProxyEnable=1 指向本工具端口，会让下面的「已接管」
# 瞬间为真。只在「没有实例在跑」时才动它——此时这些设置确实没人负责。
$snapProxy = Get-ProxySnapshot
$staleLeftover = ($snapProxy.Enable -eq 1) -and ($snapProxy.Server -eq $listen) -and ((Get-TongtuProcesses).Count -eq 0)
if ($staleLeftover) {
    Write-Host "[2/7] 发现自代理残留（ProxyEnable=1 指向 $listen，但没有实例在跑），先清干净。" -ForegroundColor Yellow
    New-ItemProperty -Path $regPath -Name "ProxyEnable" -Value 0 -PropertyType DWord -Force | Out-Null
    $snapProxy = Get-ProxySnapshot
}
$snapHosts = if (Test-Path $hostsPath) { [IO.File]::ReadAllBytes($hostsPath) } else { $null }
Write-Host ("[2/7] 基线: ProxyEnable={0} ProxyServer='{1}'；hosts 托管块={2}；管理员={3}" -f `
    $snapProxy.Enable, $snapProxy.Server, (Test-HostsHasManagedBlock), $admin)

$failures = New-Object System.Collections.Generic.List[string]

# —— 3. 启动并等「接管 + RunOnce 已登记」——
Write-Host "[3/7] 启动并等待接管…" -ForegroundColor Gray
$proc = Start-Process -FilePath $Exe -ArgumentList 'start', '--no-tray', '--hidden', '--no-hosts', '--no-mitm' -PassThru
$mainPid = $proc.Id
$sw = [Diagnostics.Stopwatch]::StartNew()
$armed = $false
$ready = $false
while ($sw.Elapsed.TotalSeconds -lt $WaitSec) {
    $now = Get-ProxySnapshot
    $runOnce = Get-RunOnceValue
    if (($now.Enable -eq 1) -and ($runOnce -eq $runOnceWant) -and (Test-PortOpen $control)) { $ready = $true; break }
    if ($proc.HasExited) { break }
    Start-Sleep -Milliseconds 300
}
if ($proc.HasExited) {
    Write-Host "结论：无法测试。主进程启动后就退出了（先单独跑一次 $Exe status 看原因）。" -ForegroundColor Red
    exit 2
}
if (-not $ready) {
    $now = Get-ProxySnapshot
    Write-Host ("结论：无法测试。没等到「系统代理接管 + RunOnce 登记」：ProxyEnable={0}，RunOnce='{1}'。" -f `
        $now.Enable, (Get-RunOnceValue)) -ForegroundColor Yellow
    Write-Host "      若配置里 system_proxy: false，本项无从验证。" -ForegroundColor Yellow
    & taskkill.exe /F /T /PID $mainPid 2>&1 | Out-Null
    exit 2
}
$armed = (Get-RunOnceValue) -eq $runOnceWant
Write-Host ("[3/7] 主进程 pid={0}；用时 {1}s；系统代理已接管；RunOnce='{2}'" -f `
    $mainPid, [math]::Round($sw.Elapsed.TotalSeconds, 1), (Get-RunOnceValue)) -ForegroundColor Gray

# —— 4. 模拟断电：先杀监护进程，再杀主进程 ——
# 顺序很重要：先杀掉监护进程，它就没机会在主进程退出后还原（真掉电时它同样来不及）。
Write-Host "[4/7] 模拟断电：先杀监护进程，再杀主进程…" -ForegroundColor Gray
$all = Get-TongtuProcesses
$guards = @($all | Where-Object { $_.Id -ne $mainPid })
foreach ($g in $guards) { & taskkill.exe /F /PID $g.Id 2>&1 | Out-Null }
Start-Sleep -Milliseconds 200
& taskkill.exe /F /PID $mainPid 2>&1 | Out-Null
$deadline = (Get-Date).AddSeconds(10)
while ((Get-TongtuProcesses).Count -gt 0 -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 200 }
Start-Sleep -Milliseconds 800

$dirtyProxy = Get-ProxySnapshot
$dirtyGuard = Test-Path $guardState
if (-not (($dirtyProxy.Enable -eq 1) -and $dirtyProxy.Server -eq $listen)) {
    Write-Host "结论：无法模拟断电。杀进程后系统代理已被还原（监护进程抢在杀它之前完成了兜底），本项跳过。" -ForegroundColor Yellow
    if (-not $SkipRestore) { Set-ProxySnapshot $snapProxy }
    exit 2
}
if (-not $dirtyGuard) {
    Write-Host "      注意：guard.json 已不在（主进程可能在临死前回收了它），继续验证系统代理与 RunOnce。" -ForegroundColor DarkYellow
}
Write-Host ("      脏状态已就位：ProxyEnable={0} → {1}；RunOnce={2}" -f `
    $dirtyProxy.Enable, $dirtyProxy.Server, (Get-RunOnceValue)) -ForegroundColor Gray

# —— 5. 像登录时那样执行一次 __recover ——
Write-Host "[5/7] 执行 tongtu __recover（Windows 登录时就是这么调的）…" -ForegroundColor Gray
$recover = Start-Process -FilePath $Exe -ArgumentList '__recover' -Wait -PassThru
$recoverCode = $recover.ExitCode
Start-Sleep -Milliseconds 500

# —— 6. 断言 ——
$now = Get-ProxySnapshot
$runOnceAfter = Get-RunOnceValue
$guardLeft = Test-Path $guardState
$hostsBlock = Test-HostsHasManagedBlock
$procs = Get-TongtuProcesses

if ($recoverCode -ne 0) { $failures.Add("__recover 退出码 $recoverCode（期望 0）") }
if (($now.Enable -ne $snapProxy.Enable) -or ($now.Server -ne $snapProxy.Server)) {
    $failures.Add(("系统代理没还原: ProxyEnable={0} ProxyServer='{1}'，应为 ProxyEnable={2} ProxyServer='{3}'" -f `
        $now.Enable, $now.Server, $snapProxy.Enable, $snapProxy.Server))
}
if ($runOnceAfter -ne "") { $failures.Add("RunOnce 项没被清除: '$runOnceAfter'") }
if ($guardLeft -and -not $dirtyGuard) { $failures.Add("多出了 guard.json 残留") }
if ($hostsBlock) { $failures.Add("hosts 里残留托管块（$beginMark）") }
if ($procs.Count -gt 0) { $failures.Add("__recover 之后还有通途进程没退出: pid $($procs.Id -join ', ')") }

# 再跑一次：没有残留状态时必须幂等（退出码 0、不改动任何东西）
$again = Start-Process -FilePath $Exe -ArgumentList '__recover' -Wait -PassThru
if ($again.ExitCode -ne 0) { $failures.Add("重复执行 __recover 退出码 $($again.ExitCode)（期望 0）") }
if ((Get-RunOnceValue) -ne "") { $failures.Add("重复执行 __recover 又写回了 RunOnce 项") }

Write-Host "[6/7] 结果" -ForegroundColor Gray
Write-Host ""
Write-Host "  检查项                        期望                         实际" -ForegroundColor Cyan
$rows = @(
    @{ Name = "__recover 退出码";   Want = "0";                                      Got = "$recoverCode"; Ok = ($recoverCode -eq 0) },
    @{ Name = "系统代理已还原";     Want = "ProxyEnable=$($snapProxy.Enable)";        Got = "ProxyEnable=$($now.Enable)"; Ok = (($now.Enable -eq $snapProxy.Enable) -and ($now.Server -eq $snapProxy.Server)) },
    @{ Name = "RunOnce 项已清除";   Want = "无";                                      Got = $(if ($runOnceAfter) { $runOnceAfter } else { "无" }); Ok = ($runOnceAfter -eq "") },
    @{ Name = "guard.json 已回收";  Want = "无";                                      Got = $(if ($guardLeft) { "有" } else { "无" }); Ok = (-not $guardLeft) },
    @{ Name = "hosts 无托管块";     Want = "无";                                      Got = $(if ($hostsBlock) { "有" } else { "无" }); Ok = (-not $hostsBlock) },
    @{ Name = "无通途进程残留";     Want = "0 个";                                    Got = "$($procs.Count) 个"; Ok = ($procs.Count -eq 0) }
)
foreach ($r in $rows) {
    $mark = if ($r.Ok) { "OK  " } else { "FAIL" }
    $color = if ($r.Ok) { "Green" } else { "Red" }
    Write-Host ("  [{0}] {1,-26} {2,-28} {3}" -f $mark, $r.Name, $r.Want, $r.Got) -ForegroundColor $color
}
Write-Host ""

# 日志取证：__recover 的结论要能在日志里看到
Write-Host "[7/7] 日志取证（$logFile）:" -ForegroundColor Gray
if (Test-Path $logFile) {
    $tail = Get-Content -Path $logFile -Tail 60 -ErrorAction SilentlyContinue
    $hit = @($tail | Where-Object { $_ -match "断电自愈|遗留的系统代理|托管块" })
    if ($hit.Count -eq 0) { Write-Host "    没在日志末尾找到断电自愈相关记录（日志可能被轮转了）" -ForegroundColor DarkYellow }
    $hit | Select-Object -Last 4 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
} else {
    Write-Host "    日志文件不存在（log.file 被关掉了？）" -ForegroundColor DarkYellow
}
Write-Host ""

if ($failures.Count -eq 0) {
    Write-Host "结论：通过。模拟断电后的脏状态被 __recover 还原干净，且重复执行无副作用。" -ForegroundColor Green
} else {
    Write-Host "结论：未通过。" -ForegroundColor Red
    foreach ($f in $failures) { Write-Host "      - $f" -ForegroundColor Red }
}
Write-Host ""
Write-Host "补充说明：RunOnce 以当前用户权限运行，提权会话留下的 hosts 托管块它清不掉；" -ForegroundColor DarkGray
Write-Host "          那种情况会写警告，并在「以管理员身份启动的下一次通途」启动时清掉。" -ForegroundColor DarkGray

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
