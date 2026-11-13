# start-soak.ps1 — 在后台启动 24 小时浸泡测试，并让它脱离当前终端 / 会话。
#
# 为什么还要一个启动脚本：
#   直接在当前终端里 Start-Process 的话，终端（或包着它的作业对象、IDE、CI 会话）一结束，
#   浸泡进程可能被一起收走。改用「计划任务」承载：进程由任务计划服务拉起，
#   既不挂在当前终端下，也不依赖某个控制台窗口——2026-11-02 与 2026-11-04 两次
#   都恰好跑到 28 分钟就被 Ctrl+C 打断（CSV 停在 t=1680s），换成计划任务是为了避开这类
#   只影响「某个控制台」的外部打断。
#
# 用法：
#   .\tools\start-soak.ps1                         # 用 dist\tongtu.exe，跑满 24 小时（计划任务）
#   .\tools\start-soak.ps1 -Hours 0.05             # 3 分钟快速验证脚本本身
#   .\tools\start-soak.ps1 -NoMITM                 # 只浸泡系统代理路径（不启动反代）
#   .\tools\start-soak.ps1 -Stop                   # 停止正在跑的浸泡实例并还原状态
#   .\tools\start-soak.ps1 -ViaProcess             # 旧方式：WMI 直接拉进程（调试脚本用）
#
# 注意：
#   - 会把 dist\tongtu.exe 复制到 %LOCALAPPDATA%\tongtu-soak\<版本>\，避免后续重新构建覆盖
#     正在运行的 exe；不放 %TEMP%：字节相同的副本在临时目录里更容易被杀软按启发式隔离。
#   - 计划任务名固定为 TongtuSoak，每次启动都会先删掉旧的再注册；浸泡结束后任务留在
#     「就绪」状态，不会自己再跑（触发器是一次性的）。
#   - 启动后会等第一份 CSV 样本落盘再返回，失败时直接打印错误日志。
#   - 结束时（24 小时到点）由 soak-test.ps1 正常停止实例并还原系统代理。
param(
    [string]$Exe = "",
    [string]$Version = "",
    [double]$Hours = 24,
    [int]$IntervalSec = 60,
    [switch]$NoMITM,
    [switch]$ViaProcess,
    [switch]$Stop
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
if (-not $Exe) { $Exe = Join-Path $root "dist\tongtu.exe" }
if (-not (Test-Path $Exe)) { throw "找不到可执行文件: $Exe（先执行 .\build.ps1）" }
$Exe = (Resolve-Path $Exe).Path

$ver = $Version.Trim()
if (-not $ver) {
    # 优先读仓库根目录的 VERSION（唯一权威来源），而不是执行一次 exe 问它：
    # 少一次进程启动，就少一次被杀软按行为启发式盯上的机会；本地构建的版本号本来就来自这里。
    $vf = Join-Path $root "VERSION"
    if (Test-Path $vf) { $ver = (Get-Content $vf -Raw).Trim() }
}
if (-not $ver) {
    $ver = (& $Exe version 2>$null | Select-Object -First 1)
    if ($ver) { $ver = ($ver -replace '^tongtu\s+', '').Trim() } else { $ver = "unknown" }
}
$dir = Join-Path $env:LOCALAPPDATA "tongtu-soak\$ver"
$pidFile = Join-Path $dir "soak.pid"
$csvHint = Join-Path $dir "soak.csvpath"
$taskName = "TongtuSoak"

if ($Stop) {
    if (Test-Path $csvHint) {
        $csv = (Get-Content $csvHint -Raw).Trim()
        Write-Host "停止浸泡：$csv" -ForegroundColor Yellow
    }
    # 先让通途正常退出：它会自己还原系统代理 / RunOnce。
    # 顺序不能反：先停任务会把浸泡脚本直接杀掉，它的 finally 就来不及收尾。
    $copy = Join-Path $dir "tongtu.exe"
    if (Test-Path $copy) {
        & $copy stop 2>$null | Out-Null
    }
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
        Write-Host "已移除计划任务 $taskName。" -ForegroundColor Gray
    }
    if (Test-Path $pidFile) {
        $old = (Get-Content $pidFile -Raw).Trim()
        if ($old -match '^\d+$') {
            Get-Process -Id ([int]$old) -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
        }
    }
    Write-Host "已请求停止；系统代理与 RunOnce 由通途的退出流程还原。" -ForegroundColor Gray
    exit 0
}

New-Item -ItemType Directory -Force -Path $dir | Out-Null
Copy-Item $Exe (Join-Path $dir "tongtu.exe") -Force

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$csv = Join-Path $dir "soak-$stamp.csv"
$out = Join-Path $dir "soak-$stamp.out.log"
$err = Join-Path $dir "soak-$stamp.err.log"
$soak = Join-Path $root "tools\soak-test.ps1"
$pwshCmd = (Get-Command pwsh -ErrorAction SilentlyContinue)
$pwsh = if ($pwshCmd) { $pwshCmd.Source } else { (Get-Process -Id $PID).Path }

$soakArgs = @(
    "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "`"$soak`"",
    "-Exe", "`"$(Join-Path $dir 'tongtu.exe')`"",
    "-Hours", "$Hours", "-IntervalSec", "$IntervalSec", "-CsvPath", "`"$csv`""
)
if ($NoMITM) { $soakArgs += "-NoMITM" }

# 把「pwsh + 重定向」写进一个 .cmd：比在计划任务里拼多层引号可靠得多，
# 也方便出问题时直接双击这个文件复现。
$runner = Join-Path $dir "run-soak-$stamp.cmd"
$inner = "`"$pwsh`" " + ($soakArgs -join " ")
# 注意括号：PowerShell 里逗号的优先级高于 +，写成 @('@echo off', $a + '...') 
# 会先组成数组再逐段「追加」，把一条命令行拆成 7 行（已踩过）。
$runLine = $inner + ' > "' + $out + '" 2> "' + $err + '"'
@('@echo off', $runLine) | Set-Content -Path $runner -Encoding ascii

$mode = "计划任务"
if ($ViaProcess) {
    $mode = "WMI 进程"
    # 旧方式：cmd 负责重定向。cmd /c 的经典写法是「外层一对引号 + 内部可执行文件自己的引号」：
    #   cmd.exe /c ""C:\Program Files\PowerShell\7\pwsh.exe" ... > "out" 2> "err""
    # 少一层 / 多一层都会让 cmd 把 ""C:\Program 当成一个非法命令名（已踩过）。
    $cmdLine = 'cmd.exe /c "' + $inner + ' > "' + $out + '" 2> "' + $err + '""'
    $res = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
        CommandLine      = $cmdLine
        CurrentDirectory = $root
    }
    if ($res.ReturnValue -ne 0) {
        throw "后台进程启动失败（Win32_Process.Create 返回 $($res.ReturnValue)）"
    }
} else {
    # 计划任务：由任务计划服务拉起，进程不挂在当前终端 / 控制台下。
    # 执行时限留 30 分钟余量，避免到点被服务强行结束；电池/休眠设置都放开，
    # 否则笔记本一拔电，任务就会被默认策略停掉。
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
    $action = New-ScheduledTaskAction -Execute $env:COMSPEC -Argument ('/c "' + $runner + '"') -WorkingDirectory $root
    $trigger = New-ScheduledTaskTrigger -Once -At ((Get-Date).AddMinutes(1))
    $limit = New-TimeSpan -Minutes ([math]::Ceiling($Hours * 60) + 30)
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
        -DontStopOnIdleEnd -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit $limit
    $principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" `
        -LogonType Interactive -RunLevel Limited
    Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger `
        -Settings $settings -Principal $principal -Description "通途长时间浸泡测试" | Out-Null
    Start-ScheduledTask -TaskName $taskName
}

# 真正的 pwsh 是任务 / cmd 的子进程；按命令行找它并记下 PID（-Stop 用）。
$soakPid = 0
for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 500
    $p = Get-CimInstance Win32_Process -Filter "Name = 'pwsh.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -like "*soak-test.ps1*" -and $_.CommandLine -like "*$stamp*" } |
        Select-Object -First 1
    if ($p) { $soakPid = [int]$p.ProcessId; break }
}
if ($soakPid -eq 0) {
    Write-Host "后台进程已启动，但没能确认 PID；仍可按 CSV 是否存在判断运行状态。" -ForegroundColor Yellow
} else {
    Set-Content -Path $pidFile -Value $soakPid -Encoding ascii
}
$csv | Set-Content -Path $csvHint -Encoding utf8

Write-Host "通途浸泡已启动" -ForegroundColor Green
Write-Host "  版本:   $ver"
Write-Host "  方式:   $mode"
Write-Host "  PID:    $(if ($soakPid) { $soakPid } else { '未知' })"
Write-Host "  CSV:    $csv"
Write-Host "  日志:   $out"
Write-Host "  时长:   $Hours 小时（每 $IntervalSec 秒采样）"
Write-Host "  接管:   系统代理=开，反代=$(if ($NoMITM) { '关' } else { '开' })"
Write-Host ""

# 等到第一份样本 + 启动行落盘，确认不是“启动即失败”。
$ok = $false
for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 500
    if ((Test-Path $csv) -and (Get-Item $csv).Length -gt 0) { $ok = $true; break }
}
if (-not $ok) {
    Write-Host "60 秒内没有 CSV 样本，启动可能失败。错误日志：" -ForegroundColor Red
    if (Test-Path $err) { Get-Content $err -Tail 30 }
    exit 1
}

Write-Host "首个样本已落盘：" -ForegroundColor Gray
Get-Content $csv -Tail 2 | ForEach-Object { "  $_" }
Write-Host ""
Write-Host "查看进度: Get-Content '$csv' -Tail 3" -ForegroundColor Gray
Write-Host "停止浸泡: .\tools\start-soak.ps1 -Stop" -ForegroundColor Gray
