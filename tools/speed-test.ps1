# speed-test.ps1 — 加速效果验收：同一批白名单域名、同一时刻，直连 vs 经本地代理的真实 HTTPS 对照。
#
# 为什么要跑这个：
#   `tongtu test` 只回答「通不通」，回答不了「是不是真的更快」。
#   这个脚本对每个目标各发一次真实 HTTPS 请求，量两个数：
#     - TTFB：发出请求到收到响应头的耗时（最贴近「网页点开卡不卡」）
#     - 速率：读满一个固定窗口的吞吐（MB/s，体现传输阶段是否被拖慢）
#   两条通路交替测量（直连 → 经代理 → 直连 → 经代理），避免网络波动偏袒某一方：
#     - 直连：UseProxy=false，带真 SNI，完全绕开系统代理与本地代理；
#     - 经代理：走 127.0.0.1:19180，即通途接管后浏览器实际走的那条路。
#   另加一路「系统代理通道」：用默认（读系统代理设置）的客户端发请求，
#   验证「接管之后，普通应用不改任何配置就能走通途」。
#
# 用法：
#   .\tools\speed-test.ps1                     # 用 dist\tongtu.exe 的默认监听，跑默认目标
#   .\tools\speed-test.ps1 -Rounds 3 -WindowMB 4
#   .\tools\speed-test.ps1 -Targets github.com,raw.githubusercontent.com
#
# 前置：
#   需要通途正在运行并已接管系统代理（脚本会先检查，没跑就直接退出，不擅自启动）。
param(
    [string]$Listen = "127.0.0.1:19180",
    [string[]]$Targets = @(
        # 覆盖「典型被阻断」与「通常可通」两类，避免只挑好测的
        "store.steampowered.com",
        "steamcommunity.com",
        "github.com",
        "raw.githubusercontent.com",
        "cdn.steamstatic.com"
    ),
    [int]$Rounds = 2,          # 每种通路各测几轮（取最小值＝能力上限，减少抖动）
    [int]$WindowMB = 2,        # 吞吐窗口：读满多少 MB 计时
    [int]$TimeoutSec = 12
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Net.Http

$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings"

function Test-PortOpen([string]$addr) {
    $p = $addr.Split(":")
    $c = New-Object System.Net.Sockets.TcpClient
    try { return $c.ConnectAsync($p[0], [int]$p[1]).Wait(500) } catch { return $false } finally { $c.Close() }
}

# —— 前置检查 ——
$k = Get-ItemProperty -Path $regPath -ErrorAction SilentlyContinue
$proxyUp = Test-PortOpen $Listen
$sysProxyOn = ($k.ProxyEnable -eq 1) -and ($k.ProxyServer -eq $Listen)

Write-Host "通途加速效果验收（直连 vs 经代理）" -ForegroundColor Cyan
Write-Host "  目标: $($Targets -join ', ')"
Write-Host "  通路: 直连 / 经 $Listen / 系统代理通道；每路 $Rounds 轮，窗口 ${WindowMB}MB"
Write-Host ""
if (-not $proxyUp) {
    Write-Host "本地代理 $Listen 没在监听——先执行 .\dist\tongtu.exe start 再跑本脚本。" -ForegroundColor Red
    exit 2
}
if (-not $sysProxyOn) {
    Write-Host "系统代理未指向 $Listen（ProxyEnable=$($k.ProxyEnable) ProxyServer='$($k.ProxyServer)），" -ForegroundColor Yellow
    Write-Host "「系统代理通道」这一路测出来会不准；直连/经代理两路不受影响。" -ForegroundColor Yellow
}
Write-Host ""

function New-ProxiedClient {
    $h = [System.Net.Http.HttpClientHandler]::new()
    $h.Proxy = [System.Net.WebProxy]::new("http://$Listen")
    $h.UseProxy = $true
    $c = [System.Net.Http.HttpClient]::new($h)
    $c.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
    return $c
}
function New-DirectClient {
    $h = [System.Net.Http.HttpClientHandler]::new()
    $h.UseProxy = $false          # 关键：绕开系统代理与本地代理，测真实直连
    $c = [System.Net.Http.HttpClient]::new($h)
    $c.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
    return $c
}
function New-SystemClient {
    $h = [System.Net.Http.HttpClientHandler]::new()
    $h.UseProxy = $true           # 不指定 Proxy：读系统代理设置＝浏览器的默认行为
    $h.Proxy = $null
    $c = [System.Net.Http.HttpClient]::new($h)
    $c.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
    return $c
}

# 发一次 GET，量 TTFB 与「读满 windowBytes」的吞吐。返回 @{TTFB;MBps;Bytes;Err}
function Invoke-Probe($client, [string]$hostName, [int]$windowBytes) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $req = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Get, "https://$hostName/")
    $req.Headers.Host = $hostName
    try {
        $resp = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        $ttfb = $sw.Elapsed.TotalMilliseconds
        $status = [int]$resp.StatusCode
        $stream = $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $buf = New-Object byte[] 65536
        $total = 0
        $cts = [System.Threading.CancellationTokenSource]::new()
        $cts.CancelAfter($TimeoutSec * 1000)
        while ($total -lt $windowBytes) {
            $n = $stream.ReadAsync($buf, 0, $buf.Length, $cts.Token).GetAwaiter().GetResult()
            if ($n -le 0) { break }
            $total += $n
        }
        $sw.Stop()
        $resp.Dispose(); $req.Dispose()
        $secs = $sw.Elapsed.TotalSeconds
        $mbps = if ($secs -gt 0 -and $total -gt 0) { [math]::Round(($total / 1MB) / $secs, 2) } else { 0 }
        return @{ TTFB = [math]::Round($ttfb, 0); MBps = $mbps; Bytes = $total; Status = $status; Err = "" }
    } catch {
        $e = $_.Exception
        while ($e.InnerException) { $e = $e.InnerException }
        $msg = $e.Message -replace "\s+", " "
        if ($msg.Length -gt 46) { $msg = $msg.Substring(0, 46) + "…" }
        $req.Dispose()
        return @{ TTFB = -1; MBps = -1; Bytes = 0; Status = 0; Err = $msg }
    }
}

$windowBytes = $WindowMB * 1MB
$dirClient = New-DirectClient
$prxClient = New-ProxiedClient
$sysClient = New-SystemClient

# 收集各通路每轮结果：$results[目标][通路] = @(@{...}, ...)
$results = [ordered]@{}
foreach ($t in $Targets) { $results[$t] = @{ direct = @(); proxy = @(); system = @() } }

# —— 交替测量：直连 → 经代理，逐轮打穿，避免时间差带来的偏差 ——
for ($r = 1; $r -le $Rounds; $r++) {
    foreach ($t in $Targets) {
        $results[$t]["direct"] += Invoke-Probe $dirClient $t $windowBytes
        $results[$t]["proxy"]  += Invoke-Probe $prxClient $t $windowBytes
        Write-Host ("  轮{0}  {1,-32} 直连 {2} / 经代理 {3}" -f $r, $t,
            $(if ($results[$t]["direct"][-1].Err) { "失败" } else { "$($results[$t]['direct'][-1].TTFB)ms $($results[$t]['direct'][-1].MBps)MB/s" }),
            $(if ($results[$t]["proxy"][-1].Err) { "失败" } else { "$($results[$t]['proxy'][-1].TTFB)ms $($results[$t]['proxy'][-1].MBps)MB/s" })) -ForegroundColor DarkGray
    }
}
# 系统代理通道只测一轮（它和「经代理」本质同一条路，只验证配置真的生效）
foreach ($t in $Targets) {
    $results[$t]["system"] += Invoke-Probe $sysClient $t $windowBytes
}

# 取每路最小值：代表「这条通路的最好成绩」，抖动只会让它变差、不会变好
function Best($arr, [string]$field) {
    $vals = $arr | Where-Object { $_.Err -eq "" -and $_.$field -ge 0 } | ForEach-Object { $_.$field }
    if (-not $vals) { return $null }
    return ($vals | Measure-Object -Minimum).Minimum
}
function OkCount($arr) { @($arr | Where-Object { $_.Err -eq "" }).Count }

Write-Host ""
Write-Host "对照结果（每路取 $Rounds 轮里的最好成绩；直连=绕开一切代理，经代理=走通途）" -ForegroundColor Cyan
Write-Host ""
Write-Host ("  {0,-32} {1,-16} {2,-16} {3,-14} {4}" -f "目标", "直连 TTFB/速率", "经代理 TTFB/速率", "TTFB 提速", "系统代理通道") -ForegroundColor Cyan

$rows = @()
foreach ($t in $Targets) {
    $dOk = OkCount $results[$t]["direct"]; $pOk = OkCount $results[$t]["proxy"]; $sOk = OkCount $results[$t]["system"]
    $dT = Best $results[$t]["direct"] "TTFB"; $pT = Best $results[$t]["proxy"] "TTFB"
    $dB = Best $results[$t]["direct"] "MBps"; $pB = Best $results[$t]["proxy"] "MBps"
    $sT = Best $results[$t]["system"] "TTFB"; $sB = Best $results[$t]["system"] "MBps"

    $dTxt = if ($null -eq $dT) { "失败 ($dOk/$Rounds)" } else { "$dT ms / $dB MB/s" }
    $pTxt = if ($null -eq $pT) { "失败 ($pOk/$Rounds)" } else { "$pT ms / $pB MB/s" }
    $sTxt = if ($null -eq $sT) { "失败" } else { "通 ($sT ms)" }

    $speedup = "-"
    if (($null -ne $dT) -and ($null -ne $pT) -and ($pT -gt 0)) {
        $x = [math]::Round($dT / $pT, 2)
        $speedup = if ($x -ge 1.05) { "${x}x" } elseif ($x -le 0.95) { "${x}x" } else { "持平" }
    } elseif (($null -eq $dT) -and ($null -ne $pT)) {
        $speedup = "直连不通"
    }
    $rows += [pscustomobject]@{
        Target = $t; DirTxt = $dTxt; PrxTxt = $pTxt; Speedup = $speedup; SysTxt = $sTxt
        DirOK = ($null -ne $dT); PrxOK = ($null -ne $pT); SysOK = ($null -ne $sT)
        DirT = $dT; PrxT = $pT
    }
    Write-Host ("  {0,-32} {1,-16} {2,-16} {3,-14} {4}" -f $t, $dTxt, $pTxt, $speedup, $sTxt) -ForegroundColor $(
        if ($null -ne $pT) { "Green" } else { "Red" })
}

# —— 结论 ——
$prxOkN = @($rows | Where-Object PrxOK).Count
$dirOkN = @($rows | Where-Object DirOK).Count
$sysOkN = @($rows | Where-Object SysOK).Count
$faster = @($rows | Where-Object { $_.Speedup -ne "-" -and $_.Speedup -ne "持平" -and $_.Speedup -ne "直连不通" })
$blockedByDirect = @($rows | Where-Object { $_.Speedup -eq "直连不通" })
$total = $rows.Count

Write-Host ""
$failures = @()
if ($prxOkN -lt $total) { $failures += "经代理仍有 $($total - $prxOkN)/$total 个目标不通——加速没有全覆盖" }
if ($sysOkN -lt $total) { $failures += "系统代理通道只有 $sysOkN/$total 通——接管后浏览器会用不了" }

if ($failures.Count -gt 0) {
    Write-Host "结论：未通过。" -ForegroundColor Red
    foreach ($f in $failures) { Write-Host "  - $f" -ForegroundColor Red }
    exit 1
}

Write-Host "结论：通过。经代理 $prxOkN/$total 全通，系统代理通道 $sysOkN/$total 全通。" -ForegroundColor Green
$evidence = @()
if ($blockedByDirect.Count -gt 0) {
    $evidence += "直连根本打不开的有 $($blockedByDirect.Count) 个（$($blockedByDirect.Target -join '、')），经代理全通——加速是真实生效的"
}
if ($faster.Count -gt 0) {
    $evidence += "另有 $($faster.Count) 个直连虽通但更慢（$($faster.Target -join '、')）"
}
if ($evidence.Count -eq 0) {
    $evidence += "本机网络下直连与经代理表现相当（当前网络环境未对这些目标设障），通路本身工作正常"
}
foreach ($e in $evidence) { Write-Host "  · $e" -ForegroundColor Green }
Write-Host ""

$dirClient.Dispose(); $prxClient.Dispose(); $sysClient.Dispose()
exit 0
