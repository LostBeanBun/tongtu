# soak-test.ps1 — 长时间运行浸泡测试（默认 24 小时）：看它会不会越跑越大、悄悄死掉或丢掉接管。
#
# 为什么要跑这个：
#   「能启动、能加速」在几分钟内就能验证；「连续跑一天还不漏内存、不丢句柄、不把系统代理
#   丢在半路」只能靠时间。0.4.0 的主题就是「跑得久、坏得少」，所以把它做成可重复的脚本：
#   按固定间隔采样主进程的内存 / 句柄 / 线程 / CPU / TCP 连接、日志轮转情况与接管状态，
#   边采样边写 CSV，最后再判断「有没有异常增长」。
#
# 用法：
#   .\tools\soak-test.ps1                          # 跑满 24 小时，每 60 秒采一次
#   .\tools\soak-test.ps1 -Hours 0.05 -IntervalSec 10   # 3 分钟快速验证脚本本身
#   .\tools\soak-test.ps1 -NoHosts -NoMITM         # 只浸泡系统代理路径（不动 hosts / 反代）
#   .\tools\soak-test.ps1 -CsvPath D:\tmp\soak.csv # 指定 CSV 输出位置
#
# 注意：
#   - 脚本会启动一个 `--no-tray --hidden` 实例并让它接管系统代理（默认也含 hosts / 反代），
#     期间这台电脑的流量就走通途——这正是「真实长期运行」的场景，建议就在日常使用中跑。
#   - 结束时会正常停止实例并还原系统代理 / hosts，不会把机器留在半接管状态。
#   - 判定阈值是经验值：内存增长 >150 MB 或句柄增长 >300 视为失败（先 WARN 后 FAIL），
#     长时间空闲运行出现这种增长通常意味着有泄漏。
param(
    [string]$Exe = "",
    [double]$Hours = 24,
    [int]$IntervalSec = 60,
    [string]$CsvPath = "",
    [int]$WarmupSamples = 5,     # 前几个样本算启动热身（缓存/预热），不计入基线
    [switch]$NoHosts,
    [switch]$NoMITM,
    [switch]$RestartAfter        # 测完重新启动一个实例（若跑之前本来就在运行）
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
if (-not $Exe) { $Exe = Join-Path $root "dist\tongtu.exe" }
if (-not (Test-Path $Exe)) { throw "找不到可执行文件: $Exe（先执行 .\build.ps1）" }
$Exe = (Resolve-Path $Exe).Path
if ($Hours -le 0) { throw "-Hours 必须大于 0" }
if ($IntervalSec -lt 1) { throw "-IntervalSec 至少 1 秒" }

$stamp = Get-Date -Format "yyyyMMdd-HHmmss"
if (-not $CsvPath) {
    $CsvPath = Join-Path $env:TEMP "tongtu-soak\soak-$stamp.csv"
}
$csvDir = Split-Path -Parent $CsvPath
if ($csvDir) { New-Item -ItemType Directory -Force -Path $csvDir | Out-Null }

$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings"
$runOnceKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\RunOnce"
$runOnceName = "TongtuRecover"
$dataDir = Join-Path $env:APPDATA "tongtu"
$cfgPath = Join-Path $dataDir "config.yaml"
$hostsPath = Join-Path $env:WINDIR "System32\drivers\etc\hosts"
$beginMark = "# >>> tongtu managed block >>>"

$listen = "127.0.0.1:19180"
$control = "127.0.0.1:19181"
if (Test-Path $cfgPath) {
    $cfgText = Get-Content -Path $cfgPath -Raw
    if ($cfgText -match "(?m)^\s*listen:\s*(\S+)") { $listen = $Matches[1].Trim('"', "'") }
    if ($cfgText -match "(?m)^\s*control:\s*(\S+)") { $control = $Matches[1].Trim('"', "'") }
}
$proxyPort = [int]($listen.Split(":")[1])

$handler = [System.Net.Http.SocketsHttpHandler]::new()
$handler.UseProxy = $false
$http = [System.Net.Http.HttpClient]::new($handler)
$http.Timeout = [TimeSpan]::FromSeconds(5)

function Get-ProxySnapshot {
    $k = Get-ItemProperty -Path $regPath -ErrorAction SilentlyContinue
    [pscustomobject]@{
        Enable = if ($null -ne $k.ProxyEnable) { [int]$k.ProxyEnable } else { 0 }
        Server = [string]$k.ProxyServer
    }
}

function Set-ProxySnapshot($snap) {
    New-ItemProperty -Path $regPath -Name "ProxyEnable" -Value $snap.Enable -PropertyType DWord -Force | Out-Null
    if ($snap.Server) {
        New-ItemProperty -Path $regPath -Name "ProxyServer" -Value $snap.Server -PropertyType String -Force | Out-Null
    } else {
        Remove-ItemProperty -Path $regPath -Name "ProxyServer" -ErrorAction SilentlyContinue
    }
}

function Get-Status {
    try {
        $json = $http.GetStringAsync("http://$control/status").GetAwaiter().GetResult()
        return $json | ConvertFrom-Json
    } catch {
        return $null
    }
}

function Get-TongtuProcesses { @(Get-Process -Name tongtu -ErrorAction SilentlyContinue) }

function Test-HostsHasManagedBlock {
    if (-not (Test-Path $hostsPath)) { return $false }
    return [bool](Get-Content -Path $hostsPath -ErrorAction SilentlyContinue |
        Where-Object { $_ -like "*$beginMark*" } | Select-Object -First 1)
}

function Get-RunOnceValue {
    $k = Get-ItemProperty -Path $runOnceKey -Name $runOnceName -ErrorAction SilentlyContinue
    if ($null -eq $k) { return "" }
    return [string]$k.$runOnceName
}

function Get-TcpEstablished([int]$port) {
    try {
        return @(Get-NetTCPConnection -LocalPort $port -State Established -ErrorAction SilentlyContinue).Count
    } catch {
        return -1
    }
}

function Get-LogStats {
    $files = @(Get-ChildItem -Path $dataDir -Filter "tongtu.log*" -ErrorAction SilentlyContinue)
    $mb = 0.0
    foreach ($f in $files) { $mb += $f.Length / 1MB }
    return [pscustomobject]@{ Files = $files.Count; MB = [math]::Round($mb, 2) }
}

function Wait-Until([scriptblock]$cond, [int]$sec) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $sec) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

Write-Host "通途长时间运行浸泡测试" -ForegroundColor Cyan
Write-Host ("  exe:      {0}" -f $Exe)
Write-Host ("  时长:     {0} 小时（每 {1} 秒采样）" -f $Hours, $IntervalSec)
Write-Host ("  CSV:      {0}" -f $CsvPath)
Write-Host ("  接管范围: 系统代理=开，hosts={0}，反代={1}" -f `
    $(if ($NoHosts) { "关" } else { "开" }), $(if ($NoMITM) { "关" } else { "开" }))
Write-Host ""

$failures = New-Object System.Collections.Generic.List[string]
$warnings = New-Object System.Collections.Generic.List[string]
$proc = $null
$writer = $null
$snapProxy = $null
$wasRunning = $false
$startTakeover = $false
$samples = New-Object System.Collections.Generic.List[object]

try {
    # —— 1. 让已有实例正常退出（不强杀别人的会话）——
    $wasRunning = (Get-TongtuProcesses).Count -gt 0
    if ($wasRunning) {
        Write-Host "[1/4] 检测到已有实例，先正常退出…" -ForegroundColor Yellow
        Start-Process -FilePath $Exe -ArgumentList "stop" -Wait | Out-Null
        if (-not (Wait-Until { (Get-TongtuProcesses).Count -eq 0 } 15)) {
            Write-Host "结论：无法测试。已有实例没有正常退出。" -ForegroundColor Red
            exit 2
        }
    } else {
        Write-Host "[1/4] 没有正在运行的通途实例。" -ForegroundColor Gray
    }

    $snapProxy = Get-ProxySnapshot
    $snapHosts = if (Test-Path $hostsPath) { [IO.File]::ReadAllBytes($hostsPath) } else { $null }

    # —— 2. 启动 ——
    # 注意别用 $args：它是 PowerShell 的自动变量（未绑定参数），拿来存命令行容易出怪问题。
    $startArgs = @("start", "--no-tray", "--hidden")
    if ($NoHosts) { $startArgs += "--no-hosts" }
    if ($NoMITM) { $startArgs += "--no-mitm" }
    Write-Host "[2/4] 启动并等待接管…" -ForegroundColor Gray
    $proc = Start-Process -FilePath $Exe -ArgumentList $startArgs -PassThru
    $mainPid = $proc.Id
    $ready = Wait-Until {
        $st = Get-Status
        return ($null -ne $st) -and $st.running -and ($st.system_proxy -or $st.hosts)
    } 60
    if ($proc.HasExited) {
        Write-Host "结论：无法测试。主进程启动后就退出了。" -ForegroundColor Red
        exit 2
    }
    if (-not $ready) {
        Write-Host "结论：无法测试。60 秒内没有接管成功（检查 config.yaml 的 system_proxy / hosts）。" -ForegroundColor Red
        exit 2
    }
    $st0 = Get-Status
    $startTakeover = [bool]$st0.system_proxy
    $startHosts = [bool]$st0.hosts
    Write-Host ("      就绪：pid={0} 系统代理={1} hosts={2} 反代={3}；RunOnce={4}" -f `
        $mainPid, $st0.system_proxy, $st0.hosts, $st0.mitm,
        $(if (Get-RunOnceValue) { "已登记" } else { "未登记" })) -ForegroundColor Gray

    # —— 3. 采样 ——
    $writer = [System.IO.StreamWriter]::new($CsvPath, $false, (New-Object System.Text.UTF8Encoding($false)))
    $writer.WriteLine("time,elapsed_s,ws_mb,private_mb,handles,threads,cpu_s,tcp_est,guards,log_files,log_mb,runonce,proxy_on,uptime_s")
    $writer.Flush()

    $total = [TimeSpan]::FromHours($Hours)
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $nextAt = 0.0
    $count = 0
    Write-Host "[3/4] 开始采样（Ctrl+C 可提前结束并给出结论）…" -ForegroundColor Gray
    while ($sw.Elapsed -lt $total) {
        if ($sw.Elapsed.TotalSeconds -lt $nextAt) {
            Start-Sleep -Milliseconds 250
            continue
        }
        $nextAt += $IntervalSec

        if ($proc.HasExited) {
            $failures.Add("主进程在第 $count 个样本附近退出了（退出码 $($proc.ExitCode)）")
            break
        }
        $proc.Refresh()
        $st = Get-Status
        $logs = Get-LogStats
        $runOnce = Get-RunOnceValue
        $row = [pscustomobject]@{
            Time     = (Get-Date).ToString("s")
            Elapsed  = [math]::Round($sw.Elapsed.TotalSeconds, 1)
            WSMB     = [math]::Round($proc.WorkingSet64 / 1MB, 1)
            Private  = [math]::Round($proc.PrivateMemorySize64 / 1MB, 1)
            Handles  = [int]$proc.HandleCount
            Threads  = [int]$proc.Threads.Count
            CPUSec   = [math]::Round($proc.TotalProcessorTime.TotalSeconds, 1)
            TcpEst   = Get-TcpEstablished $proxyPort
            Guards   = [Math]::Max(0, (Get-TongtuProcesses).Count - 1)
            LogFiles = $logs.Files
            LogMB    = $logs.MB
            RunOnce  = $(if ($runOnce) { 1 } else { 0 })
            ProxyOn  = $(if ($st) { [int][bool]$st.system_proxy } else { -1 })
            Uptime   = $(if ($st) { [int64]$st.uptime_sec } else { -1 })
        }
        $samples.Add($row)
        $writer.WriteLine(("{0},{1},{2},{3},{4},{5},{6},{7},{8},{9},{10},{11},{12},{13}" -f `
            $row.Time, $row.Elapsed, $row.WSMB, $row.Private, $row.Handles, $row.Threads,
            $row.CPUSec, $row.TcpEst, $row.Guards, $row.LogFiles, $row.LogMB, $row.RunOnce,
            $row.ProxyOn, $row.Uptime))
        $writer.Flush()

        Write-Host ("      [{0}] t={1,7:N0}s  内存 {2,6:N1} MB  句柄 {3,5}  线程 {4,3}  TCP {5,3}  日志 {6,4:N1} MB  uptime {7}" -f `
            $count, $row.Elapsed, $row.WSMB, $row.Handles, $row.Threads, $row.TcpEst, $row.LogMB, $row.Uptime)
        $count++

        if ($null -eq $st) {
            $failures.Add("第 $count 个样本时控制端口无响应（服务可能已卡死）")
            break
        }
        if ($startTakeover -and -not $st.system_proxy) {
            $failures.Add("系统代理接管在第 $count 个样本时丢了（ProxyNote=$($st.proxy_note)）")
            break
        }
        if ($startHosts -and -not $st.hosts) {
            $failures.Add("hosts 接管在第 $count 个样本时丢了")
            break
        }
    }
    $writer.Dispose()
    $writer = $null
    $sw.Stop()

    # —— 4. 结论 ——
    Write-Host ""
    Write-Host "[4/4] 结论" -ForegroundColor Gray
    if ($samples.Count -lt 2) {
        $failures.Add("样本太少（$($samples.Count) 个），无法判断趋势")
    } else {
        $skip = [Math]::Min($WarmupSamples, [Math]::Max(0, $samples.Count - 1))
        $steady = @($samples | Select-Object -Skip $skip)
        $tailCount = [Math]::Max(1, [int]($steady.Count * 0.2))
        $tail = @($steady | Select-Object -Last $tailCount)

        $baseWS = ($steady | Select-Object -First 1).WSMB
        $baseHandles = ($steady | Select-Object -First 1).Handles
        $baseTcp = ($steady | Select-Object -First 1).TcpEst
        $lastWS = ($tail | Measure-Object -Property WSMB -Average).Average
        $lastHandles = ($tail | Measure-Object -Property Handles -Average).Average
        $maxTcp = ($tail | Measure-Object -Property TcpEst -Maximum).Maximum
        $maxLogMB = ($samples | Measure-Object -Property LogMB -Maximum).Maximum
        $minGuards = ($steady | Measure-Object -Property Guards -Minimum).Minimum

        $wsGrowth = [math]::Round($lastWS - $baseWS, 1)
        $handleGrowth = [int]($lastHandles - $baseHandles)
        $tcpGrowth = if ($baseTcp -ge 0 -and $maxTcp -ge 0) { [int]($maxTcp - $baseTcp) } else { 0 }

        Write-Host ("  样本: {0} 个 / {1:N1} 分钟；平均采样间隔 {2:N1}s" -f `
            $samples.Count, ($sw.Elapsed.TotalMinutes), ($sw.Elapsed.TotalSeconds / $samples.Count))
        Write-Host ("  内存: 基线 {0:N1} MB → 末段均值 {1:N1} MB（变化 {2:+0.0;-0.0} MB）" -f `
            $baseWS, $lastWS, $wsGrowth)
        Write-Host ("  句柄: 基线 {0} → 末段均值 {1:N0}（变化 {2:+0;-0}）" -f `
            $baseHandles, $lastHandles, $handleGrowth)
        Write-Host ("  TCP:  基线 {0} → 末段峰值 {1}（变化 {2:+0;-0}）" -f $baseTcp, $maxTcp, $tcpGrowth)
        Write-Host ("  日志: 最多 {0:N1} MB / {1} 个文件（单文件上限 5 MB，保留 3 份）" -f `
            $maxLogMB, ($samples | Measure-Object -Property LogFiles -Maximum).Maximum)

        if ($proc.HasExited) {
            $failures.Add("进程已退出（退出码 $($proc.ExitCode)）")
        } else {
            $st = Get-Status
            if ($null -eq $st -or -not $st.running) {
                $failures.Add("结束时控制端口无响应")
            } elseif ($startTakeover -and -not $st.system_proxy) {
                $failures.Add("结束时系统代理未处于接管状态")
            }
            if ($startTakeover -and -not (Get-RunOnceValue)) {
                $failures.Add("结束时断电自愈项不在了（异常掉电就没有兜底）")
            }
        }
        if ($minGuards -lt 1) {
            $failures.Add("采样期间监护进程消失过（崩溃防护出现缺口）")
        }

        if ($wsGrowth -gt 150) {
            $failures.Add("内存增长 $wsGrowth MB（>150 MB，疑似泄漏）")
        } elseif ($wsGrowth -gt 80) {
            $warnings.Add("内存增长 $wsGrowth MB（>80 MB，建议留意）")
        }
        if ($handleGrowth -gt 300) {
            $failures.Add("句柄增长 $handleGrowth（>300，疑似泄漏）")
        } elseif ($handleGrowth -gt 150) {
            $warnings.Add("句柄增长 $handleGrowth（>150，建议留意）")
        }
        # 这里的连接数是「本地代理端口上的 established」：包含通途自己预热后保留的
        # keep-alive 连接（实测会稳定在几十个），也包含其他程序经系统代理的连接。
        # 所以阈值放得比较宽——真正要看的是「持续单调增长」。
        if ($tcpGrowth -gt 200) {
            $failures.Add("经本地代理的连接增长 $tcpGrowth（>200，可能有连接没关）")
        } elseif ($tcpGrowth -gt 60) {
            $warnings.Add("经本地代理的连接增长 $tcpGrowth（>60，建议留意）")
        }
    }

    foreach ($w in $warnings) { Write-Host "  注意：$w" -ForegroundColor DarkYellow }
    if ($failures.Count -eq 0) {
        Write-Host "结论：通过。长时间运行未见异常增长，接管状态与断电自愈项都还在。" -ForegroundColor Green
    } else {
        Write-Host "结论：未通过。" -ForegroundColor Red
        foreach ($f in $failures) { Write-Host "      - $f" -ForegroundColor Red }
    }
    Write-Host "CSV 已保存: $CsvPath" -ForegroundColor Gray
} finally {
    if ($null -ne $writer) { $writer.Dispose() }
    # —— 收尾：停实例、还原系统状态 ——
    try {
        if ($null -ne $proc -and -not $proc.HasExited) {
            Start-Process -FilePath $Exe -ArgumentList "stop" -Wait | Out-Null
            if (-not (Wait-Until { (Get-TongtuProcesses).Count -eq 0 } 20)) {
                Get-TongtuProcesses | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
                Write-Host "实例没能在 20 秒内正常退出，已强制结束。" -ForegroundColor Yellow
            }
        }
        if ($null -ne $snapProxy) {
            $cur = Get-ProxySnapshot
            if ($cur.Enable -ne $snapProxy.Enable -or $cur.Server -ne $snapProxy.Server) {
                Set-ProxySnapshot $snapProxy
                Write-Host "已还原系统代理为启动之前的状态。" -ForegroundColor Gray
            }
        }
        if ($null -ne $snapHosts -and (Test-HostsHasManagedBlock)) {
            try {
                [IO.File]::WriteAllBytes($hostsPath, $snapHosts)
                Write-Host "已还原 hosts 为启动之前的内容。" -ForegroundColor Gray
            } catch {
                Write-Host "hosts 还原失败（需要管理员权限）：$($_.Exception.Message)" -ForegroundColor Yellow
            }
        }
        if ((Get-RunOnceValue) -ne "") {
            Remove-ItemProperty -Path $runOnceKey -Name $runOnceName -ErrorAction SilentlyContinue
            Write-Host "清掉了残留的断电自愈项。" -ForegroundColor Gray
        }
        if ($RestartAfter -and $wasRunning) {
            Start-Process -FilePath $Exe | Out-Null
            Write-Host "已重新启动通途（跑之前它本来就在运行）。" -ForegroundColor Gray
        }
    } catch {
        Write-Host "收尾时出错：$($_.Exception.Message)" -ForegroundColor Red
    }
}

if ($failures.Count -gt 0) { exit 1 }
exit 0
