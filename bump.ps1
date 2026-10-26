# bump.ps1 — 统一修改版本号（VERSION 文件 + cmd/tongtu/version.go）
#
# 用法:
#   .\bump.ps1 patch      # 0.0.1 -> 0.0.2   修 bug、小改动、文档整理
#   .\bump.ps1 minor      # 0.0.2 -> 0.1.0   新增功能、行为有变化
#   .\bump.ps1 major      # 0.4.0 -> 1.0.0   达到可发布状态
#   .\bump.ps1 0.0.7      # 直接指定版本号
#
# 版本号规则（见 CHANGELOG.md 开头）：
#   - 0.0.x        开发期，接口与配置都可能随时变，不保证兼容
#   - 0.x.y        功能成型但未定稿：新增功能抬 minor，修复抬 patch
#   - 1.0.0        可发布：配置格式与命令行为承诺向后兼容
#
# 只改版本号不会提交任何东西；改完记得补 CHANGELOG.md 并提交推送（tag 只给重要版本打）。
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$Target
)

$ErrorActionPreference = "Stop"

$root = $PSScriptRoot
$versionFile = Join-Path $root "VERSION"
$versionGo = Join-Path $root "cmd\tongtu\version.go"

if (-not (Test-Path $versionFile)) { throw "找不到 $versionFile" }
if (-not (Test-Path $versionGo)) { throw "找不到 $versionGo" }

$current = (Get-Content $versionFile -Raw).Trim()
if ($current -notmatch '^(\d+)\.(\d+)\.(\d+)$') { throw "当前 VERSION 不是 x.y.z 形式: $current" }
$maj = [int]$Matches[1]; $min = [int]$Matches[2]; $pat = [int]$Matches[3]

switch ($Target.ToLower()) {
    "patch" { $new = "$maj.$min." + ($pat + 1) }
    "minor" { $new = "$maj." + ($min + 1) + ".0" }
    "major" { $new = ($maj + 1) + ".0.0" }
    default {
        if ($Target -notmatch '^(\d+)\.(\d+)\.(\d+)$') {
            throw "参数要 patch / minor / major，或者一个 x.y.z 版本号；收到: $Target"
        }
        $new = $Target
    }
}
if ($new -eq $current) { throw "版本号没有变化: $current" }

# VERSION 是单行纯文本、不带行尾（与仓库里的既有格式一致）：
# Set-Content 会补 CRLF，既产生无意义的 diff，也会被 git diff --check 判成行尾空白。
[System.IO.File]::WriteAllText($versionFile, $new, [System.Text.Encoding]::ASCII)

# 同步 version.go 里的默认值（保留其余内容与缩进）
$go = Get-Content $versionGo -Raw
$newGo = [regex]::Replace($go, 'var Version = "\d+\.\d+\.\d+"', "var Version = `"$new`"")
if ($newGo -eq $go) { throw "没能在 version.go 里找到 var Version 声明" }
Set-Content -Path $versionGo -Value $newGo -Encoding UTF8 -NoNewline

Write-Host "版本号: $current -> $new" -ForegroundColor Green
Write-Host "  已更新 VERSION 与 cmd\tongtu\version.go" -ForegroundColor Gray
Write-Host ""
Write-Host "别忘了：" -ForegroundColor Yellow
Write-Host "  1. 在 CHANGELOG.md 顶部补这一版的条目"
Write-Host "  2. .\build.ps1  重新构建（会自动读取 VERSION）"
Write-Host "  3. git add -A && git commit -m `"版本 $new`" && git push"
Write-Host "     （v0.0.x 日常迭代不打 tag；重要版本再加 `"git tag -a v$new -m ...`" 并推送标签）" -ForegroundColor Gray
