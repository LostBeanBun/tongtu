# release-notes.ps1 — 从 CHANGELOG.md 里取出指定版本的条目，拼成 GitHub Release 说明。
#
# 为什么单独一个脚本：发布流程里最容易「人肉复制漏一段」的就是 Release 说明。
# 让 CI 用同一份 CHANGELOG 生成，版本条目与仓库里的记录就不会对不上。
#
# 用法：
#   .\tools\release-notes.ps1 -Version 0.5.0                      # 打印到屏幕
#   .\tools\release-notes.ps1 -Version 0.5.0 -OutFile notes.md   # 写入文件
param(
    [Parameter(Mandatory = $true)]
    [string]$Version,
    [string]$Changelog = "",
    [string]$OutFile = ""
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
if (-not $Changelog) { $Changelog = Join-Path $root "CHANGELOG.md" }
if (-not (Test-Path $Changelog)) { throw "找不到 $Changelog" }

$text = Get-Content -Path $Changelog -Raw
$pattern = "(?ms)^## \[$([regex]::Escape($Version))\][^\n]*\n.*?(?=^## \[|\z)"
$m = [regex]::Match($text, $pattern)
if (-not $m.Success) {
    throw "CHANGELOG.md 里没有 [$Version] 的条目；先补 CHANGELOG 再发版。"
}
$section = $m.Value.TrimEnd()

# 下载与校验说明放在最前面：用户点进 Release 第一眼要知道下哪个、怎么确认没被改过。
$header = @'
Windows x64 绿色单文件：下载后直接运行，无需安装，不写系统目录。

**校验下载**（PowerShell）：

```powershell
Get-FileHash .\ASSET -Algorithm SHA256
# 与 SHA256SUMS.txt 里的值比对
```

---

'@ -replace 'ASSET', "tongtu-$Version-windows-x64.exe"

$notes = $header + "`n" + $section + "`n"

if ($OutFile) {
    $dir = Split-Path -Parent $OutFile
    if ($dir) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    Set-Content -Path $OutFile -Value $notes -Encoding utf8
    Write-Host "已写入 $OutFile（$([math]::Round($notes.Length / 1KB, 1)) KB）" -ForegroundColor Gray
} else {
    Write-Output $notes
}
