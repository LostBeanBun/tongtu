# build.ps1 — 编译 Tongtu（Windows x64 单文件）
# 用法:
#   .\build.ps1                 # 版本号自动读取仓库根目录的 VERSION
#   .\build.ps1 -Version 0.2.0  # 临时指定版本号（不改 VERSION 文件）
#
# 改版本号请用 .\bump.ps1（同时更新 VERSION 与 version.go，见 CHANGELOG.md 开头的规则）。
#
# 关键：-H windowsgui 让 exe 走「图形子系统」。
# 这样双击时 Windows 根本不会给通途创建 cmd 窗口（不是隐藏，是没有），
# 也就不存在「关掉 cmd 窗口 → 进程被系统结束」的问题。
# 从终端运行时，程序自己用 AttachConsole 接到父进程的控制台，
# CLI 输出照常可见（见 cmd/tongtu/console_windows.go）。
param(
    [string]$Version = "",
    [string]$Output  = "dist\tongtu.exe"
)

$ErrorActionPreference = "Stop"

# 默认版本号来自 VERSION（单一权威来源；version_test.go 会检查它没跟代码漂移）
if (-not $Version) {
    $vf = Join-Path $PSScriptRoot "VERSION"
    if (Test-Path $vf) {
        $Version = (Get-Content $vf -Raw).Trim()
    } else {
        $Version = "0.0.0-dev"
    }
}
if ($Version -notmatch '^\d+\.\d+\.\d+') { throw "版本号格式不对: $Version" }

# Go 工具链（若未在 PATH 中，尝试默认安装位置）
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $env:PATH = "$env:LOCALAPPDATA\Programs\Go\bin;$env:PATH"
}
if (-not $env:GOPROXY) { $env:GOPROXY = "https://goproxy.cn,direct" }

$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"

New-Item -ItemType Directory -Force -Path (Split-Path $Output) | Out-Null

Write-Host "编译中（v$Version）..." -ForegroundColor Cyan
go build -trimpath -ldflags "-s -w -H windowsgui -X main.Version=$Version" -o $Output .\cmd\tongtu
if ($LASTEXITCODE -ne 0) { throw "编译失败" }

$f = Get-Item $Output
Write-Host ("编译完成: {0}" -f $f.FullName) -ForegroundColor Green
Write-Host ("  大小: {0:N2} MB" -f ($f.Length / 1MB))
Write-Host ("  版本: {0}" -f $Version)
Write-Host "  子系统: GUI（双击不出现 cmd 窗口）"
