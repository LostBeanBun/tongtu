# add-defender-exclusion.ps1 — 给开发目录加 Defender 排除项（需要管理员，由 UAC 提权运行）。
#
# 为什么必须有这一步：
#   通途接管系统代理、写自启与 RunOnce、拉起监护进程的组合行为会被
#   Defender 的启发式判成 Behavior:Win32/Impact.A!ml（ID 2147776683），
#   未签名的本地构建会在接管瞬间被隔离，导致浸泡与界面验收无法进行。
#   这只解决本机开发验证，不解决面向用户的误报（那是发布侧的问题，见 CHANGELOG）。
#
# 结果写到 $(Split-Path $PSScriptRoot)/exclusion.log，供非提权会话读取。
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$log = Join-Path $PSScriptRoot "defender-exclusion.log"
$root = Split-Path -Parent $PSScriptRoot
$paths = @(
    $root,
    (Join-Path $env:LOCALAPPDATA "tongtu-soak")
)
try {
    $before = @((Get-MpPreference).ExclusionPath)
    foreach ($p in $paths) {
        Add-MpPreference -ExclusionPath $p
    }
    $after = @((Get-MpPreference).ExclusionPath)
    $lines = @(
        "time=" + (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'),
        "added=" + ($paths -join '; '),
        "before=" + ($before -join '; '),
        "after=" + ($after -join '; '),
        "result=OK"
    )
    Set-Content -Path $log -Value $lines -Encoding utf8
} catch {
    Set-Content -Path $log -Value @("time=" + (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), "result=FAIL", "error=" + $_.Exception.Message) -Encoding utf8
    throw
}
