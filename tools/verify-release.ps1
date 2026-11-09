# verify-release.ps1 — 发布后核验：下载线上附件，逐项比对大小 / SHA-256 / exe 自报版本。
#
# 为什么需要它：
#   本地构建成功、CI 构建成功，都不等于「上传后的字节没坏、校验和没写错、exe 里的版本号没退回」。
#   用户实际拿到的是 Release 页面上的附件，所以这里只信线上数据：
#   从 GitHub API 取 release → 下载附件 → 与 SHA256SUMS.txt、资产 digest、exe 自报版本三方对齐。
#
# 用法：
#   .\tools\verify-release.ps1                     # 核验 VERSION 对应的发布
#   .\tools\verify-release.ps1 -Version 0.11.0     # 核验指定版本
#   .\tools\verify-release.ps1 -Keep               # 保留下载的文件（便于手工复验）
#
# 退出码：0 = 全部一致；1 = 有任一项不一致（会逐条列出）。
# 说明：需要能访问 api.github.com 与附件下载地址；不修改系统代理或任何配置。
param(
    [string]$Version = "",
    [string]$Repo = "LostBeanBun/tongtu",
    [string]$DownloadDir = "",
    [switch]$Keep,
    [int]$TimeoutSec = 60
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
if (-not $Version) {
    $verFile = Join-Path $root "VERSION"
    if (-not (Test-Path $verFile)) { throw "找不到 $verFile，请用 -Version 指定版本" }
    $Version = (Get-Content $verFile -Raw).Trim()
}
$Version = $Version.TrimStart('v')
if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "版本号要形如 0.11.0，收到: $Version" }

$tag = "v$Version"
$assetName = "tongtu-$Version-windows-x64.exe"
$sumsName = "SHA256SUMS.txt"
if (-not $DownloadDir) {
    $DownloadDir = Join-Path $env:TEMP "tongtu-verify-$Version"
}

# Windows PowerShell 5.1 默认可能仍协商旧 TLS；显式打开 1.2（PS7 下是空操作）。
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }

function Invoke-GitHub([string]$Uri) {
    $headers = @{ 'User-Agent' = 'tongtu-verify-release'; 'Accept' = 'application/vnd.github+json' }
    # CI 里用仓库自带 token 避免匿名配额（共享 runner IP 很容易撞 60 次/小时的上限）。
    # 本地运行一般没有这个环境变量，走匿名请求即可。
    $token = if ($env:GITHUB_TOKEN) { $env:GITHUB_TOKEN } elseif ($env:GH_TOKEN) { $env:GH_TOKEN } else { "" }
    if ($token) { $headers['Authorization'] = "Bearer $token" }
    $last = $null
    for ($i = 1; $i -le 3; $i++) {
        try {
            return Invoke-RestMethod -Headers $headers -Uri $Uri -TimeoutSec $TimeoutSec
        } catch {
            $last = $_
            if ($i -lt 3) { Start-Sleep -Seconds (2 * $i) }
        }
    }
    throw "请求 GitHub 失败（已重试 3 次）：$($last.Exception.Message)"
}

function Get-RemoteFile([string]$Uri, [string]$Path) {
    $headers = @{ 'User-Agent' = 'tongtu-verify-release' }
    $last = $null
    for ($i = 1; $i -le 3; $i++) {
        try {
            Invoke-WebRequest -Uri $Uri -Headers $headers -OutFile $Path -TimeoutSec 300 -UseBasicParsing
            return
        } catch {
            $last = $_
            if (Test-Path $Path) { Remove-Item $Path -Force -ErrorAction SilentlyContinue }
            if ($i -lt 3) { Start-Sleep -Seconds (2 * $i) }
        }
    }
    throw "下载失败（已重试 3 次）：$($last.Exception.Message)"
}

$results = New-Object System.Collections.Generic.List[object]
function Add-Result([string]$Name, [bool]$Ok, [string]$Detail) {
    $results.Add([pscustomobject]@{ Name = $Name; Ok = $Ok; Detail = $Detail })
}

Write-Host "通途发布核验" -ForegroundColor Cyan
Write-Host "  版本:   $Version"
Write-Host "  仓库:   $Repo"
Write-Host "  目录:   $DownloadDir"
Write-Host ""

try {
    $rel = Invoke-GitHub "https://api.github.com/repos/$Repo/releases/tags/$tag"
    Add-Result "Release 存在" ($rel.tag_name -eq $tag) "tag=$($rel.tag_name)"

    # 刚发布的版本应当就是 latest；核验旧版本时不强求（latest 本来就会更新）。
    # GitHub 的 latest 指针有几秒传播延迟，CI 里发布完立刻核验时给 30 秒余量。
    $current = (Get-Content (Join-Path $root "VERSION") -Raw).Trim()
    if ($Version -eq $current) {
        $latestTag = ""
        for ($i = 0; $i -lt 6; $i++) {
            $latest = Invoke-GitHub "https://api.github.com/repos/$Repo/releases/latest"
            $latestTag = $latest.tag_name
            if ($latestTag -eq $tag) { break }
            if ($i -lt 5) { Start-Sleep -Seconds 5 }
        }
        Add-Result "latest 指向本版本" ($latestTag -eq $tag) "latest=$latestTag"
    }

    $assets = @($rel.assets)
    $exeAsset = $assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
    $sumsAsset = $assets | Where-Object { $_.name -eq $sumsName } | Select-Object -First 1
    $available = ($assets | ForEach-Object { $_.name }) -join ", "
    Add-Result "附件 exe" ($null -ne $exeAsset) $(if ($exeAsset) { "$($exeAsset.size) 字节" } else { "缺少 $assetName（现有：$available）" })
    Add-Result "附件校验和" ($null -ne $sumsAsset) $(if ($sumsAsset) { "$($sumsAsset.size) 字节" } else { "缺少 $sumsName" })
    if ($null -eq $exeAsset -or $null -eq $sumsAsset) { throw "附件不齐，无法继续核验" }

    New-Item -ItemType Directory -Force -Path $DownloadDir | Out-Null
    $exePath = Join-Path $DownloadDir $assetName
    $sumsPath = Join-Path $DownloadDir $sumsName
    Write-Host "下载中：$assetName …" -ForegroundColor Gray
    Get-RemoteFile $exeAsset.browser_download_url $exePath
    Get-RemoteFile $sumsAsset.browser_download_url $sumsPath

    $size = (Get-Item $exePath).Length
    Add-Result "附件大小" ($size -eq [int64]$exeAsset.size) "$size / 记录 $($exeAsset.size) 字节"

    $hash = (Get-FileHash -Path $exePath -Algorithm SHA256).Hash.ToLower()
    Add-Result "附件 digest" ($exeAsset.digest -eq "sha256:$hash") "$hash / 记录 $($exeAsset.digest)"

    $sumLine = Get-Content $sumsPath | Where-Object { $_ -match [regex]::Escape($assetName) } | Select-Object -First 1
    $sumHash = if ($sumLine) { ($sumLine -split '\s+')[0].Trim().ToLower() } else { "" }
    Add-Result "SHA256SUMS.txt" ($sumHash -eq $hash) $(if ($sumHash) { $sumHash } else { "文件里没有 $assetName" })

    $out = Join-Path $DownloadDir "version.out.txt"
    $err = Join-Path $DownloadDir "version.err.txt"
    $p = Start-Process -FilePath $exePath -ArgumentList 'version' -Wait -PassThru `
        -RedirectStandardOutput $out -RedirectStandardError $err
    $printed = ((Get-Content $out -Raw -ErrorAction SilentlyContinue) + (Get-Content $err -Raw -ErrorAction SilentlyContinue)).Trim()
    $want = "tongtu $Version"
    Add-Result "exe 自报版本" ($p.ExitCode -eq 0 -and $printed -eq $want) "退出码 $($p.ExitCode)，输出 '$printed'（期望 '$want'）"
} catch {
    Add-Result "核验过程" $false $_.Exception.Message
} finally {
    if (-not $Keep) {
        Remove-Item -Recurse -Force $DownloadDir -ErrorAction SilentlyContinue
    }
}

Write-Host ""
foreach ($r in $results) {
    $mark = if ($r.Ok) { "[OK  ]" } else { "[FAIL]" }
    $color = if ($r.Ok) { "Green" } else { "Red" }
    Write-Host ("  {0} {1,-16} {2}" -f $mark, $r.Name, $r.Detail) -ForegroundColor $color
}
Write-Host ""

$failed = @($results | Where-Object { -not $_.Ok })
if ($failed.Count -eq 0) {
    Write-Host "结论：通过。线上附件与 SHA256SUMS.txt、资产 digest、exe 版本全部一致。" -ForegroundColor Green
    if ($Keep) { Write-Host "下载文件保留在: $DownloadDir" -ForegroundColor Gray }
    exit 0
}
Write-Host "结论：未通过。$($failed.Count) 项不一致。" -ForegroundColor Red
exit 1
