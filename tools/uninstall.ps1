# uninstall.ps1 — 干净卸载：把通途在系统里留下的东西逐项撤销。
#
# 为什么要单独一个脚本：
#   通途是绿色单文件，但它在系统里动过这些地方（都可能是「卸载后才发现还在」的）：
#     1. 运行中的进程（接管着系统代理 / hosts）
#     2. 开机自启（HKCU Run 值，或「最高权限」计划任务）
#     3. 断电自愈项（HKCU RunOnce 的 TongtuRecover）
#     4. 本地根证书（无 SNI 反代用，装在当前用户 Root 存储）
#     5. 系统代理（万一上次是强杀，可能还指着已经不存在的本地端口）
#     6. hosts 托管块（同上，需要管理员权限才能清）
#   手工逐项检查容易漏；这个脚本按顺序做完并逐条报告结果。
#
# 用法：
#   .\tools\uninstall.ps1                       # 撤销系统改动，保留 exe 与配置
#   .\tools\uninstall.ps1 -Purge                # 连 %APPDATA%\tongtu（配置/证书/日志）一起删
#   .\tools\uninstall.ps1 -Purge -RemoveExe     # 彻底删除（相当于「删掉 exe 与数据目录」）
#   .\tools\uninstall.ps1 -Exe D:\Tools\tongtu.exe -Purge -RemoveExe
#
#   可选：-KeepCert（保留本地根证书，例如你还想用别的工具复用）
#
# 注意：
#   - 默认**不删** exe 与配置，只撤销系统层面的改动——误删配置的代价比多点一次高。
#   - 清 hosts 托管块需要管理员权限；非管理员运行时若发现有残留，会明确告诉你该怎么做。
#   - 证书装在「当前用户」存储，不需要管理员；若当初是以别的用户身份安装的，请用那个用户运行。
param(
    [string]$Exe = "",
    [switch]$Purge,
    [switch]$RemoveExe,
    [switch]$KeepCert
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
if (-not $Exe) {
    $candidate = Join-Path $root "dist\tongtu.exe"
    if (Test-Path $candidate) { $Exe = $candidate }
}
$exeExists = ($Exe -ne "") -and (Test-Path $Exe)
if ($exeExists) { $Exe = (Resolve-Path $Exe).Path }

$runKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
$runOnceKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\RunOnce"
$runOnceName = "TongtuRecover"
$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings"
$dataDir = Join-Path $env:APPDATA "tongtu"
$cfgPath = Join-Path $dataDir "config.yaml"
$hostsPath = Join-Path $env:WINDIR "System32\drivers\etc\hosts"
$beginMark = "# >>> tongtu managed block >>>"
$endMark = "# <<< tongtu managed block <<<"

$listen = "127.0.0.1:19180"
if (Test-Path $cfgPath) {
    $cfgText = Get-Content -Path $cfgPath -Raw
    if ($cfgText -match "(?m)^\s*listen:\s*(\S+)") { $listen = $Matches[1].Trim('"', "'") }
}

$problems = New-Object System.Collections.Generic.List[string]
$notes = New-Object System.Collections.Generic.List[string]

function Get-TongtuProcesses { @(Get-Process -Name tongtu -ErrorAction SilentlyContinue) }

# Invoke-Tongtu 调用通途 exe 并可靠地拿到「标准输出 + 退出码」。
#
# 为什么不能直接 & $Exe ...：通途是 Windows GUI 子系统程序（双击不弹黑框的前提），
# 用 & 调用时 PowerShell 不会等它结束，$LASTEXITCODE 也不会被赋值——
# 之前这里就是这样误报「移除根证书失败: 」（其实证书已经删掉了）。
# Start-Process -Wait -PassThru + 重定向到临时文件是能同时拿到两者的稳妥做法。
function Invoke-Tongtu([string[]]$Arguments) {
    $outFile = [IO.Path]::GetTempFileName()
    $errFile = [IO.Path]::GetTempFileName()
    try {
        $p = Start-Process -FilePath $Exe -ArgumentList $Arguments -Wait -PassThru `
            -RedirectStandardOutput $outFile -RedirectStandardError $errFile
        $text = (@([IO.File]::ReadAllText($outFile), [IO.File]::ReadAllText($errFile)) -join "").Trim()
        return [pscustomobject]@{ Code = [int]$p.ExitCode; Output = $text }
    } finally {
        Remove-Item $outFile, $errFile -Force -ErrorAction SilentlyContinue
    }
}

function Wait-Until([scriptblock]$cond, [int]$sec) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $sec) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

function Remove-RegValue([string]$key, [string]$name) {
    if (-not (Test-Path $key)) { return $false }
    $exists = $null -ne (Get-ItemProperty -Path $key -Name $name -ErrorAction SilentlyContinue)
    if (-not $exists) { return $false }
    Remove-ItemProperty -Path $key -Name $name -Force -ErrorAction SilentlyContinue
    return $true
}

function Remove-HostsBlock {
    if (-not (Test-Path $hostsPath)) { return $false }
    $text = [IO.File]::ReadAllText($hostsPath)
    if (-not $text.Contains($beginMark)) { return $false }
    $lines = $text -split "`r?`n"
    $out = New-Object System.Collections.Generic.List[string]
    $skip = $false
    foreach ($line in $lines) {
        $t = $line.TrimEnd()
        if ($t -eq $beginMark) { $skip = $true; continue }
        if ($t -eq $endMark) { $skip = $false; continue }
        if (-not $skip) { $out.Add($line) }
    }
    $body = ($out -join "`r`n").TrimEnd() + "`r`n"
    [IO.File]::WriteAllText($hostsPath, $body, (New-Object System.Text.UTF8Encoding($false)))
    return $true
}

Write-Host "通途（Tongtu）卸载清理" -ForegroundColor Cyan
if ($exeExists) {
    Write-Host "  exe:  $Exe"
} else {
    Write-Host "  exe:  （未找到，跳过依赖 exe 的步骤）" -ForegroundColor DarkYellow
}
Write-Host "  数据: $dataDir"
Write-Host ("  选项: Purge={0} RemoveExe={1} KeepCert={2}" -f [bool]$Purge, [bool]$RemoveExe, [bool]$KeepCert)
Write-Host ""

# —— 1. 停止运行中的实例 ——
Write-Host "[1/6] 停止运行中的实例…" -ForegroundColor Gray
$procs = Get-TongtuProcesses
if ($procs.Count -eq 0) {
    Write-Host "      没有正在运行的通途。" -ForegroundColor Gray
} elseif ($exeExists) {
    Invoke-Tongtu @("stop") | Out-Null
    if (Wait-Until { (Get-TongtuProcesses).Count -eq 0 } 20) {
        Write-Host "      已正常停止并还原系统代理 / hosts。" -ForegroundColor Gray
    } else {
        $left = Get-TongtuProcesses
        Write-Host ("      正常停止超时（pid {0}），改为强制结束——系统状态可能没还原，见下一步。" -f ($left.Id -join ', ')) -ForegroundColor Yellow
        $left | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
        $notes.Add("实例是被强制结束的：系统代理 / hosts 可能残留，本脚本会在后面的步骤里兜底清理。")
    }
} else {
    $procs | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
    $notes.Add("找不到 tongtu.exe，只能强制结束进程；系统状态由后面的步骤兜底清理。")
    Write-Host "      已强制结束（没有 exe 可用来优雅停止）。" -ForegroundColor Yellow
}

# —— 2. 开机自启 ——
Write-Host "[2/6] 移除开机自启…" -ForegroundColor Gray
$removed = @()
if (Remove-RegValue $runKey "Tongtu") { $removed += "HKCU Run 项" }
$taskOut = & schtasks.exe /Query /TN "Tongtu" 2>&1
if ($LASTEXITCODE -eq 0) {
    & schtasks.exe /Delete /TN "Tongtu" /F 2>&1 | Out-Null
    if ($LASTEXITCODE -eq 0) { $removed += "计划任务 Tongtu" }
    else { $problems.Add("删除计划任务 Tongtu 失败（可能需要管理员权限）") }
}
if ($removed.Count -eq 0) {
    Write-Host "      没有找到开机自启项（本来就没开）。" -ForegroundColor Gray
} else {
    Write-Host ("      已移除: {0}" -f ($removed -join "、")) -ForegroundColor Gray
}

# —— 3. 断电自愈项 ——
Write-Host "[3/6] 移除断电自愈项…" -ForegroundColor Gray
if (Remove-RegValue $runOnceKey $runOnceName) {
    Write-Host "      已移除 RunOnce 的 TongtuRecover。" -ForegroundColor Gray
} else {
    Write-Host "      没有残留（正常退出时本来就会撤掉）。" -ForegroundColor Gray
}

# —— 4. 本地根证书 ——
Write-Host "[4/6] 处理本地根证书…" -ForegroundColor Gray
$caFile = Join-Path $dataDir "ca.crt"
if ($KeepCert) {
    Write-Host "      按 -KeepCert 保留（如需移除: tongtu cert uninstall）。" -ForegroundColor Gray
} elseif (-not (Test-Path $caFile)) {
    Write-Host "      没有找到 ca.crt（证书可能本来就删了）。" -ForegroundColor Gray
} elseif (-not $exeExists) {
    $notes.Add("找不到 tongtu.exe，无法自动移除根证书；如需要请手动在 certmgr.msc 的「受信任的根证书颁发机构」里删除 Tongtu 条目。")
    Write-Host "      跳过：没有 exe 可调用 cert uninstall。" -ForegroundColor Yellow
} else {
    $res = Invoke-Tongtu @("cert", "uninstall")
    if ($res.Code -eq 0) {
        Write-Host "      $($res.Output)" -ForegroundColor Gray
    } else {
        $problems.Add("移除根证书失败（退出码 $($res.Code)）: $($res.Output)")
    }
}

# —— 5. 系统代理与 hosts 兜底 ——
Write-Host "[5/6] 兜底还原系统代理与 hosts…" -ForegroundColor Gray
$k = Get-ItemProperty -Path $regPath -ErrorAction SilentlyContinue
$proxyPointsHere = ($null -ne $k) -and ([string]$k.ProxyServer -eq $listen)
if (($null -ne $k) -and ($k.ProxyEnable -eq 1) -and $proxyPointsHere) {
    New-ItemProperty -Path $regPath -Name "ProxyEnable" -Value 0 -PropertyType DWord -Force | Out-Null
    Write-Host "      系统代理还指着 $listen，已关闭（避免卸载后浏览器上不了网）。" -ForegroundColor Gray
} else {
    Write-Host "      系统代理没有指向 $listen，无需改动。" -ForegroundColor Gray
}
if (Test-Path $hostsPath) {
    $hostsText = [IO.File]::ReadAllText($hostsPath)
    if ($hostsText.Contains($beginMark)) {
        try {
            if (Remove-HostsBlock) {
                Write-Host "      已移除 hosts 托管块。" -ForegroundColor Gray
            }
        } catch {
            $problems.Add("移除 hosts 托管块失败（需要管理员权限）：$($_.Exception.Message)")
        }
    } else {
        Write-Host "      hosts 里没有托管块。" -ForegroundColor Gray
    }
}

# —— 6. 可选：删除数据目录与 exe ——
Write-Host "[6/6] 可选清理（配置目录 / exe）…" -ForegroundColor Gray
if ($Purge) {
    if (Test-Path $dataDir) {
        try {
            Remove-Item -Path $dataDir -Recurse -Force
            Write-Host "      已删除数据目录: $dataDir" -ForegroundColor Gray
        } catch {
            $problems.Add("删除数据目录失败: $($_.Exception.Message)")
        }
    } else {
        Write-Host "      数据目录不存在，无需删除。" -ForegroundColor Gray
    }
} else {
    Write-Host "      保留数据目录（加 -Purge 可一并删除）。" -ForegroundColor Gray
}
if ($RemoveExe) {
    if (-not $exeExists) {
        Write-Host "      没有找到要删除的 exe。" -ForegroundColor Gray
    } else {
        try {
            Remove-Item -Path $Exe -Force
            Write-Host "      已删除 exe: $Exe" -ForegroundColor Gray
        } catch {
            $problems.Add("删除 exe 失败（可能仍在运行）: $($_.Exception.Message)")
        }
    }
} else {
    Write-Host "      保留 exe（加 -RemoveExe 可一并删除）。" -ForegroundColor Gray
}

Write-Host ""
foreach ($n in $notes) { Write-Host "注意：$n" -ForegroundColor DarkYellow }
if ($problems.Count -eq 0) {
    Write-Host "结论：清理完成。通途没有在系统里留下自启、证书、代理或 hosts 改动。" -ForegroundColor Green
    exit 0
}
Write-Host "结论：有项目没清理干净：" -ForegroundColor Red
foreach ($p in $problems) { Write-Host "      - $p" -ForegroundColor Red }
exit 1
