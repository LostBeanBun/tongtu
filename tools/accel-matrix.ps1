# accel-matrix.ps1 — 加速效果归因矩阵：把「通途到底帮了什么」拆成四条通路分别量。
#
# 为什么不能只看「通/不通」：
#   网络本身会抖（同一 URL 这轮通、下轮不通），单次结果说明不了问题；
#   而且「经代理成功」可能来自三件完全不同的事——干净 DNS、换 IP、绕 SNI——
#   混在一起测就归不了因，也就说不清加速是不是真实有效。
#
# 四条通路（对同一域名、同一时刻交替测）：
#   A 系统DNS直连   ：不开通途时浏览器的路（system DNS + 真 SNI）
#   B 干净IP直连    ：DoH 干净 IP + 真 SNI，只换解析不走代理 —— 归因「DNS 污染」
#   C 经通途代理    ：系统 DNS 不参与，代理侧用干净 IP —— 产品实际走的路
#   D 无SNI出站     ：0.11 反代的出站半边，0.12.0 已删除 —— 归因「SNI 阻断」
#
# 结论按域名分类：
#   A败C胜          → 通途确实救回来了（DNS 污染 / TCP 不可达型，有效）
#   A胜C胜          → 本来就能通，通途不添乱即可（比较 C 的耗时）
#   A胜C败          → 代理反而失败（回归！必须修）
#   A败B胜C败       → 干净 IP 直连能通但代理不通（回归嫌疑）
#   A败C败D胜       → SNI 阻断，当前架构救不回（需要已删除的反代或中继）
#   全败             → 网络/目标问题，与加速无关
#
# 前置：通途正在运行；D 路需要 nosni 探针（见脚本内说明）。
param(
    [string[]]$Targets = @(
        "github.com",
        "store.steampowered.com",
        "steamcommunity.com",
        "raw.githubusercontent.com",
        "cdn.steamstatic.com"
    ),
    [int]$Rounds = 3,
    [string]$Listen = "127.0.0.1:19180",
    [int]$TimeoutSec = 10
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

# D 路探针：复刻 0.11「无 SNI 反代」的出站路径（源码 tools\nosni-probe，
# 单目标模式，退出码 0=拿到站点响应）。启动时编译一次；没有 Go 环境则 D 路记为「未测」。
$probe = $null
if (Test-Path (Join-Path $PSScriptRoot "nosni-probe\main.go")) {
    $probeBin = Join-Path $env:TEMP "tongtu-nosni-probe.exe"
    Push-Location $PSScriptRoot
    $buildOut = go build -o $probeBin ./nosni-probe 2>&1
    Pop-Location
    if ($LASTEXITCODE -eq 0 -and (Test-Path $probeBin)) { $probe = $probeBin }
}

# —— 前置：干净 DoH 解析（B 路与 D 路要用）——
function Get-CleanIP([string]$hostName) {
    foreach ($ep in @("https://223.5.5.5/resolve", "https://doh.pub/dns-query")) {
        try {
            $j = Invoke-RestMethod -Uri "$ep`?name=$hostName&type=A" -TimeoutSec 6 -Headers @{Accept='application/dns-json'}
            $ips = @($j.Answer | Where-Object { $_.type -eq 1 } | ForEach-Object { $_.data })
            if ($ips.Count -gt 0) { return $ips[0] }
        } catch { }
    }
    return $null
}
# —— 系统 DNS 解析（A 路要用，模拟不开通途时的解析结果）——
function Get-SystemIP([string]$hostName) {
    try { return ([Net.Dns]::GetHostAddresses($hostName) | Where-Object { $_.AddressFamily -eq 'InterNetwork' } | Select-Object -First 1).ToString() }
    catch { return $null }
}

function Invoke-Path($kind, [string]$hostName, [string]$ip) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $code = "000"
    switch ($kind) {
        "A" { # 系统 DNS 直连
            $r = curl.exe -s -o NUL --ssl-no-revoke --noproxy "*" --max-time $TimeoutSec -w "%{http_code}|%{time_total}" "https://$hostName/"
        }
        "B" { # 干净 IP 直连（真 SNI）
            if (-not $ip) { return @{ Ok = $false; T = -1; Code = "000" } }
            $r = curl.exe -s -o NUL --ssl-no-revoke --noproxy "*" --resolve "${hostName}:443:$ip" --max-time $TimeoutSec -w "%{http_code}|%{time_total}" "https://$hostName/"
        }
        "C" { # 经通途代理
            $r = curl.exe -s -o NUL --ssl-no-revoke -x "http://$Listen" --max-time $TimeoutSec -w "%{http_code}|%{time_total}" "https://$hostName/"
        }
    }
    $sw.Stop()
    if ($r) {
        $parts = ($r -join "") -split "\|"
        $code = $parts[0]; if ($parts.Count -gt 1 -and $parts[1]) { $t = [double]$parts[1] } else { $t = $sw.Elapsed.TotalSeconds }
    } else { $t = $sw.Elapsed.TotalSeconds }
    $ok = ($code -ne "000" -and $code -ne "")
    return @{ Ok = $ok; T = [math]::Round($t, 2); Code = $code }
}

# D 路：无 SNI 出站（已删除能力的复刻），成功与否看探针退出码
function Invoke-NoSNI([string]$hostName, [string]$ip) {
    if (-not $ip -or -not $probe -or -not (Test-Path $probe)) { return @{ Ok = $false; T = -1; Skip = $true } }
    $sw = [Diagnostics.Stopwatch]::StartNew()
    & $probe $hostName $ip *> $null
    $code = $LASTEXITCODE
    $sw.Stop()
    return @{ Ok = ($code -eq 0); T = [math]::Round($sw.Elapsed.TotalSeconds, 2); Code = "$code"; Skip = $false }
}

Write-Host "加速效果归因矩阵（$Rounds 轮取最好成绩，$TimeoutSec 秒超时）" -ForegroundColor Cyan
Write-Host "  A=系统DNS直连  B=干净IP直连  C=经通途  D=无SNI出站(0.12.0已删)" -ForegroundColor DarkGray
Write-Host ""

$clean = @{}; $system = @{}
foreach ($t in $Targets) {
    $clean[$t] = Get-CleanIP $t
    $system[$t] = Get-SystemIP $t
}

$rows = @()
foreach ($t in $Targets) {
    $best = @{ A = @{Ok=$false;T=-1}; B = @{Ok=$false;T=-1}; C = @{Ok=$false;T=-1} }
    for ($r = 1; $r -le $Rounds; $r++) {
        foreach ($k in @("A", "B", "C")) {
            $res = Invoke-Path $k $t $clean[$t]
            if ($res.Ok -and ((-not $best[$k].Ok) -or ($res.T -lt $best[$k].T) -or ($best[$k].T -lt 0))) {
                $best[$k] = $res
            } elseif (-not $best[$k].Ok) {
                $best[$k] = $res
            }
        }
    }
    $d = Invoke-NoSNI $t $clean[$t]
    $rows += [pscustomobject]@{ Host = $t; A = $best.A; B = $best.B; C = $best.C; D = $d; CleanIP = $clean[$t]; SysIP = $system[$t] }
    Write-Host ("  {0,-30} A:{1}  B:{2}  C:{3}  D:{4}" -f $t,
        $(if ($best.A.Ok) { "$($best.A.T)s" } else { "X" }),
        $(if ($best.B.Ok) { "$($best.B.T)s" } else { "X" }),
        $(if ($best.C.Ok) { "$($best.C.T)s" } else { "X" }),
        $(if ($d.Skip) { "-" } elseif ($d.Ok) { "$($d.T)s" } else { "X" })) -ForegroundColor DarkGray
}

Write-Host ""
Write-Host ("  {0,-30} {1,-17} {2,-24} {3}" -f "域名", "系统DNS→干净IP", "A直连/B干净IP/C代理/D无SNI", "归因") -ForegroundColor Cyan
$verdicts = @()
foreach ($row in $rows) {
    $dnsNote = if ($row.SysIP -and $row.CleanIP -and ($row.SysIP -eq $row.CleanIP)) { "一致" }
               elseif ($row.SysIP -and $row.CleanIP) { "不同(污染?)" }
               elseif (-not $row.SysIP) { "系统解析失败" } else { "?" }
    $abc = ("A:{0} B:{1} C:{2} D:{3}" -f `
        $(if ($row.A.Ok) {"✓"} else {"✗"}), `
        $(if ($row.B.Ok) {"✓"} else {"✗"}), `
        $(if ($row.C.Ok) {"✓"} else {"✗"}), `
        $(if ($row.D.Skip) {"-"} elseif ($row.D.Ok) {"✓"} else {"✗"}))
    # 归因
    if ($row.C.Ok -and -not $row.A.Ok) { $v = "通途救回（有效）"; $c = "Green" }
    elseif ($row.C.Ok -and $row.A.Ok) { $v = "本就可通（不添乱）"; $c = "Gray" }
    elseif (-not $row.C.Ok -and $row.A.Ok) { $v = "代理反而失败（回归！）"; $c = "Red" }
    elseif (-not $row.C.Ok -and -not $row.A.Ok -and $row.B.Ok) { $v = "干净IP通但代理不通（回归嫌疑）"; $c = "Red" }
    elseif (-not $row.C.Ok -and -not $row.A.Ok -and -not $row.B.Ok -and $row.D.Ok) { $v = "SNI阻断（架构边界，已删能力可救）"; $c = "Yellow" }
    else { $v = "全败（网络/目标侧）"; $c = "Yellow" }
    $verdicts += [pscustomobject]@{ Host = $row.Host; V = $v; C = $c }
    Write-Host ("  {0,-30} {1,-17} {2,-24} {3}" -f $row.Host, $dnsNote, $abc, $v) -ForegroundColor $c
}

Write-Host ""
Write-Host "系统DNS与干净IP对照：" -ForegroundColor Cyan
foreach ($row in $rows) {
    Write-Host ("  {0,-30} 系统={1,-16} 干净={2}" -f $row.Host, $(if ($row.SysIP) {$row.SysIP} else {"解析失败"}), $(if ($row.CleanIP) {$row.CleanIP} else {"无"})) -ForegroundColor DarkGray
}

$valid = @($verdicts | Where-Object { $_.V -eq "通途救回（有效）" }).Count
$noHarm = @($verdicts | Where-Object { $_.V -eq "本就可通（不添乱）" }).Count
$regress = @($verdicts | Where-Object { $_.V -like "*回归*" }).Count
$sni = @($verdicts | Where-Object { $_.V -like "SNI阻断*" }).Count
$dead = @($verdicts | Where-Object { $_.V -like "全败*" }).Count
Write-Host ""
Write-Host ("汇总：{0} 个目标 · 通途救回 {1} · 本就可通 {2} · 代理回归 {3} · SNI阻断(架构边界) {4} · 全败 {5}" -f `
    $rows.Count, $valid, $noHarm, $regress, $sni, $dead) -ForegroundColor Cyan
if ($regress -gt 0) { Write-Host "存在代理回归，需修复。" -ForegroundColor Red; exit 1 }
exit 0
