# ui-smoke.ps1 — 界面冒烟验收：真的去点窗口上的按钮，而不是只看接口返回值。
#
# 为什么要跑这个：
#   通途的界面是 GDI 自绘的，按钮没有 UI Automation 信息，光靠「接口能返回 paused=true」
#   证明不了「用户点得到、点得准」。0.3.0 就踩过两次只在这层才暴露的问题：
#     - 日志区右键菜单从来没弹出来（WM_CONTEXTMENU 的 wParam/lParam 取反了）；
#     - 「暂停」按钮旁边的「待办」被误点，弹出菜单卡住后续点击。
#   所以这里按用户的真实操作走一遍：
#
#     定位窗口 → 按控件 ID 取「暂停」按钮矩形 → 点它并在时长菜单里选「暂停 15 分钟」
#     → 断言系统代理 / hosts / 断电自愈项都已撤回 → 再点「恢复」→ 断言全部回来
#     → 日志区右键「复制代理地址」→ 校验剪贴板
#     → 改配置等热生效
#     → 打开诊断面板诊断 github.com，等快照里出现结论后关闭
#     → 点 × 只隐藏（进程仍在跑）→ 收尾还原。
#
# 用法：
#   .\tools\ui-smoke.ps1                    # 用 dist\tongtu.exe，截图放 %TEMP%\tongtu-ui
#   .\tools\ui-smoke.ps1 -KeepConfig        # 保留热加载用的配置改动（调试时用）
#   .\tools\ui-smoke.ps1 -SkipHotReload     # 跳过改配置那一步
#
# 注意：
#   - 脚本会移动鼠标、切换前台窗口，请在无人使用电脑时运行（交互式桌面必需）。
#   - 会快照并在结束时还原 config.yaml、hosts 与系统代理，不会留下加速状态。
#   - 非管理员运行时 hosts 本来就接管不了，相关断言会自动降级为「与启动前一致」。
param(
    [string]$Exe = "",
    [string]$ShotDir = "",
    [int]$WaitSec = 60,
    [switch]$KeepConfig,
    [switch]$SkipHotReload
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Drawing

$root = Split-Path -Parent $PSScriptRoot
if (-not $Exe) { $Exe = Join-Path $root "dist\tongtu.exe" }
if (-not (Test-Path $Exe)) { throw "找不到可执行文件: $Exe（先执行 .\build.ps1）" }
$Exe = (Resolve-Path $Exe).Path
if (-not $ShotDir) { $ShotDir = Join-Path $env:TEMP "tongtu-ui" }
New-Item -ItemType Directory -Force -Path $ShotDir | Out-Null

$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings"
$runOnceKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\RunOnce"
$runOnceName = "TongtuRecover"
$runOnceWant = '"{0}" __recover' -f $Exe
$dataDir = Join-Path $env:APPDATA "tongtu"
$cfgPath = Join-Path $dataDir "config.yaml"
$hostsPath = Join-Path $env:WINDIR "System32\drivers\etc\hosts"
$logFile = Join-Path $dataDir "tongtu.log"
$beginMark = "# >>> tongtu managed block >>>"

$listen = "127.0.0.1:19180"
$control = "127.0.0.1:19181"
if (Test-Path $cfgPath) {
    $cfgText = Get-Content -Path $cfgPath -Raw
    if ($cfgText -match "(?m)^\s*listen:\s*(\S+)") { $listen = $Matches[1].Trim('"', "'") }
    if ($cfgText -match "(?m)^\s*control:\s*(\S+)") { $control = $Matches[1].Trim('"', "'") }
}
$proxyURL = "http://$listen"

Add-Type @"
using System;
using System.Text;
using System.Runtime.InteropServices;
public class UiSmokeNative {
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool EnumChildWindows(IntPtr parent, EnumChildProc cb, IntPtr l);
  public delegate bool EnumChildProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern IntPtr SendMessage(IntPtr h, uint msg, IntPtr w, string l);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr h, ref POINT p);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint f, uint dx, uint dy, uint d, UIntPtr e);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, UIntPtr extra);
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
  [DllImport("user32.dll")] public static extern bool PostMessage(IntPtr h, uint msg, IntPtr w, IntPtr l);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
  public static IntPtr Main(uint want) {
    IntPtr found = IntPtr.Zero;
    EnumWindows((h, l) => {
      uint pid; GetWindowThreadProcessId(h, out pid);
      if (pid == want && IsWindowVisible(h)) {
        var sb = new StringBuilder(256); GetWindowText(h, sb, 256);
        if (sb.Length > 0) { found = h; return false; }
      }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  // 诊断面板打开时，可见的 EDIT 子控件就是它的域名输入框：
  // 日志区是 RichEdit（类名不同），站点面板的输入框此时被隐藏。
  public static IntPtr VisibleEdit(IntPtr parent) {
    IntPtr found = IntPtr.Zero;
    EnumChildWindows(parent, (h, l) => {
      var sb = new StringBuilder(64); GetClassName(h, sb, 64);
      if (string.Equals(sb.ToString(), "Edit", StringComparison.OrdinalIgnoreCase) && IsWindowVisible(h)) {
        found = h; return false;
      }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  // 直接给输入框发 WM_SETTEXT：模拟键盘逐字输入对「点得准」没有额外价值，
  // 而中文输入法与键盘布局会让逐字输入不稳定。
  public static void SetText(IntPtr h, string s) { SendMessage(h, 0x000C, IntPtr.Zero, s); }
  // SetForegroundWindow 对「不是当前前台进程」的调用有诸多限制；
  // 先按一下 Alt 再调用，Windows 会认为用户正在操作，前台切换才稳定。
  public static void Force(IntPtr h) {
    keybd_event(0x12, 0, 0, UIntPtr.Zero);
    keybd_event(0x12, 0, 2, UIntPtr.Zero);
    SetForegroundWindow(h);
  }
  public static void Click(int x, int y) {
    SetCursorPos(x, y);
    System.Threading.Thread.Sleep(150);
    mouse_event(0x0002, 0, 0, 0, UIntPtr.Zero);
    System.Threading.Thread.Sleep(60);
    mouse_event(0x0004, 0, 0, 0, UIntPtr.Zero);
  }
  public static void RightClick(int x, int y) {
    SetCursorPos(x, y);
    System.Threading.Thread.Sleep(150);
    mouse_event(0x0008, 0, 0, 0, UIntPtr.Zero);
    System.Threading.Thread.Sleep(60);
    mouse_event(0x0010, 0, 0, 0, UIntPtr.Zero);
  }
  public static void Key(byte vk) {
    keybd_event(vk, 0, 0, UIntPtr.Zero);
    keybd_event(vk, 0, 2, UIntPtr.Zero);
    System.Threading.Thread.Sleep(150);
  }
}
"@

$script:failures = New-Object System.Collections.Generic.List[string]
$script:notes = New-Object System.Collections.Generic.List[string]
$script:proc = $null
$script:cfgBackup = $null
$script:hostsBackup = $null
$script:cfgChanged = $false

$handler = [System.Net.Http.SocketsHttpHandler]::new()
$handler.UseProxy = $false
$script:http = [System.Net.Http.HttpClient]::new($handler)
$script:http.Timeout = [TimeSpan]::FromSeconds(3)

function Shot($hwnd, [string]$path) {
    $wr = New-Object UiSmokeNative+RECT
    [UiSmokeNative]::GetWindowRect($hwnd, [ref]$wr) | Out-Null
    $w = $wr.Right - $wr.Left; $h = $wr.Bottom - $wr.Top
    $bmp = New-Object System.Drawing.Bitmap($w, $h)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $hdc = $g.GetHdc()
    # PrintWindow 的 PW_RENDERFULLCONTENT：被其他窗口遮挡时也能抓到真实内容
    [UiSmokeNative]::PrintWindow($hwnd, $hdc, 2) | Out-Null
    $g.ReleaseHdc($hdc); $g.Dispose()
    if ($path) { $bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png) }
    return $bmp
}

function Get-ProxyEnable { [int](Get-ItemProperty $regPath).ProxyEnable }

function Get-RunOnceValue {
    $k = Get-ItemProperty -Path $runOnceKey -Name $runOnceName -ErrorAction SilentlyContinue
    if ($null -eq $k) { return "" }
    return [string]$k.$runOnceName
}

function Test-HostsHasManagedBlock {
    if (-not (Test-Path $hostsPath)) { return $false }
    return [bool](Get-Content -Path $hostsPath -ErrorAction SilentlyContinue |
        Where-Object { $_ -like "*$beginMark*" } | Select-Object -First 1)
}

# 往 config.yaml 的 custom 段里加一个域名（热加载检查用）。
#
# 不能简单地在 "custom:" 后面插一行：应用生成的配置里已经有 domains: []，
# 再接一行会变成重复键，YAML 直接解析失败（0.10.x 冒烟就在这里误报过）。
# 这里按行处理，兼容空列表 / 已有内联列表 / 块状列表三种写法。
function Add-SmokeCustomDomain([string]$text, [string]$domain) {
    $nl = "`r`n"
    $lines = New-Object 'System.Collections.Generic.List[string]'
    [void]$lines.AddRange([string[]]($text -split "\r?\n"))

    $customIdx = -1
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match '^custom:\s*$') { $customIdx = $i; break }
    }
    if ($customIdx -lt 0) {
        $lines.Insert(0, "custom:")
        $lines.Insert(1, "  domains: ['$domain']")
        return ($lines -join $nl)
    }

    # custom 段到下一个顶层键（或文件末尾）为止。
    $end = $lines.Count
    for ($j = $customIdx + 1; $j -lt $lines.Count; $j++) {
        if ($lines[$j] -match '^\S') { $end = $j; break }
    }

    $indent = "  "
    for ($j = $customIdx + 1; $j -lt $end; $j++) {
        # 沿用段内已有的缩进：同一层的键必须对齐，否则 YAML 会把它算进上一个值里。
        if ($lines[$j] -match '^(\s+)\S') { $indent = $Matches[1]; break }
    }
    for ($j = $customIdx + 1; $j -lt $end; $j++) {
        if ($lines[$j] -match '^(\s+)domains:\s*(.*)$') {
            $indent = $Matches[1]
            $val = $Matches[2].Trim()
            if ($val -eq "") {
                # 块状列表：沿用已有条目的缩进（YAML 要求同一列表项对齐），
                # 没有条目时按常见的两格缩进补。
                $itemIndent = "$indent  "
                for ($k = $j + 1; $k -lt $end; $k++) {
                    if ($lines[$k] -match '^(\s+)-\s') { $itemIndent = $Matches[1]; break }
                    if ($lines[$k] -match '^\s*\S') { break }
                }
                $lines.Insert($j + 1, "$itemIndent- '$domain'")
            } elseif ($val -match '^\[(.*)\]$') {
                $inner = $Matches[1].Trim()
                $list = if ($inner) { "'$domain', $inner" } else { "'$domain'" }
                $lines[$j] = "${indent}domains: [$list]"
            } else {
                # 不认识的写法（正常配置不会出现）：不猜测，让热加载如实报错。
                $script:notes.Add("custom.domains 的写法无法安全追加，跳过热加载检查")
            }
            return ($lines -join $nl)
        }
    }
    # custom 段里还没有 domains：插在第一行子项之前。
    $lines.Insert($customIdx + 1, "${indent}domains: ['$domain']")
    return ($lines -join $nl)
}

function Get-Status {
    # 直接用 HttpClient 且关掉代理：恢复接管后 ProxyEnable=1 指向 127.0.0.1，
    # Invoke-RestMethod 默认会读系统代理设置，可能把「查自己」的请求也塞进代理里。
    try {
        $json = $script:http.GetStringAsync("http://$control/status").GetAwaiter().GetResult()
        return $json | ConvertFrom-Json
    } catch {
        return $null
    }
}

function Get-UILayout {
    try {
        $json = $script:http.GetStringAsync("http://$control/ui/layout").GetAwaiter().GetResult()
        return $json | ConvertFrom-Json
    } catch {
        return $null
    }
}

function Get-TongtuProcesses { @(Get-Process -Name tongtu -ErrorAction SilentlyContinue) }

function Wait-Until([scriptblock]$cond, [int]$sec) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $sec) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

function Wait-Status([scriptblock]$cond, [int]$sec) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $sec) {
        $st = Get-Status
        if ($null -ne $st -and (& $cond $st)) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

# 从 /ui/layout 取控件矩形（客户区坐标）。自绘按钮没有无障碍信息，像素悬停又会
# 被状态条上每秒变化的流量计数带偏，所以由窗口自己把几何报出来（见 ui_snapshot_windows.go）。
function Get-Region([string]$id) {
    $layout = Get-UILayout
    if ($null -eq $layout) { return $null }
    return @($layout.regions | Where-Object { $_.id -eq $id }) | Select-Object -First 1
}

function Get-RegionCenter($region) {
    return @(
        [int](($region.left + $region.right) / 2),
        [int](($region.top + $region.bottom) / 2)
    )
}
Write-Host "通途界面冒烟验收" -ForegroundColor Cyan
Write-Host "  exe:      $Exe"
Write-Host "  截图目录: $ShotDir"
Write-Host "  控制端口: $control"
Write-Host ""

# —— 1. 让已有实例正常退出 ——
$wasRunning = (Get-TongtuProcesses).Count -gt 0
if ($wasRunning) {
    Write-Host "[1/10] 检测到已有实例，先正常退出…" -ForegroundColor Yellow
    Start-Process -FilePath $Exe -ArgumentList 'stop' -Wait | Out-Null
    if (-not (Wait-Until { (Get-TongtuProcesses).Count -eq 0 } 15)) {
        Write-Host "结论：无法测试。已有实例没有正常退出。" -ForegroundColor Red
        exit 2
    }
} else {
    Write-Host "[1/10] 没有正在运行的通途实例。" -ForegroundColor Gray
}

# —— 2. 快照并启动 ——
if (Test-Path $cfgPath) {
    $script:cfgBackup = [IO.File]::ReadAllBytes($cfgPath)
} else {
    throw "找不到配置文件: $cfgPath（先手动启动一次通途）"
}
$script:hostsBackup = if (Test-Path $hostsPath) { [IO.File]::ReadAllBytes($hostsPath) } else { $null }
$snapEnable = Get-ProxyEnable
$snapServer = [string](Get-ItemProperty $regPath).ProxyServer
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)

Write-Host "[2/10] 启动并等待界面就绪…" -ForegroundColor Gray
$script:proc = Start-Process -FilePath $Exe -PassThru
$mainPid = $script:proc.Id
$hwnd = [IntPtr]::Zero
$ready = Wait-Until {
    $script:st = Get-Status
    if ($null -eq $script:st) { return $false }
    $script:hwnd = [UiSmokeNative]::Main([uint32]$mainPid)
    return ($script:hwnd -ne [IntPtr]::Zero) -and $script:st.running -and ($script:st.system_proxy -or $script:st.hosts)
} $WaitSec
$hwnd = $script:hwnd
$st = $script:st
if ($script:proc.HasExited) {
    Write-Host "结论：无法测试。主进程启动后就退出了。" -ForegroundColor Red
    exit 2
}
if (-not $ready) {
    Write-Host "结论：无法测试。没等到「界面 + 接管」就绪（配置里两个接管都关着？）。" -ForegroundColor Yellow
    exit 2
}
$beforeProxy = [bool]$st.system_proxy
$beforeHosts = [bool]$st.hosts
$beforeHostsBlock = Test-HostsHasManagedBlock
Write-Host ("      窗口已就绪：系统代理={0} hosts={1}（托管块 {2}）RunOnce={3}" -f `
    $beforeProxy, $beforeHosts, $beforeHostsBlock, $(if (Get-RunOnceValue) { "已登记" } else { "无" })) -ForegroundColor Gray

# —— 3. 摆窗口、抢前台、定位「暂停」按钮 ——
# 固定窗口位置：截图与像素坐标才有可比性（60,60 时客户区原点在 (68,91)）。
[UiSmokeNative]::SetWindowPos($hwnd, [IntPtr]::Zero, 60, 60, 0, 0, 0x0001 -bor 0x0004) | Out-Null
[UiSmokeNative]::Force($hwnd)
Start-Sleep -Milliseconds 600
Shot $hwnd (Join-Path $ShotDir "01-ready.png") | Out-Null

Write-Host "[3/10] 从 /ui/layout 取「暂停」按钮矩形…" -ForegroundColor Gray
$btn = Get-Region "pause"
if ($null -eq $btn) {
    Write-Host "结论：未通过。控制端口没有给出窗口几何（窗口没创建 / 没画过？）" -ForegroundColor Red
    exit 1
}
$org = New-Object UiSmokeNative+POINT
[UiSmokeNative]::ClientToScreen($hwnd, [ref]$org) | Out-Null
$cx, $cy = Get-RegionCenter $btn
Write-Host ("      「{0}」按钮：客户区 ({1},{2})-({3},{4})，点击点 ({5},{6})" -f `
    $btn.label, $btn.left, $btn.top, $btn.right, $btn.bottom, $cx, $cy) -ForegroundColor Gray
if ($btn.label -ne "暂停") { $script:failures.Add("运行中时暂停按钮标签是 '$($btn.label)'，期望「暂停」") }

# —— 4. 暂停：系统状态 + 断电自愈项都要撤回 ——
Write-Host "[4/10] 点击「暂停」并选「暂停 15 分钟」…" -ForegroundColor Gray
[UiSmokeNative]::Force($hwnd)
Start-Sleep -Milliseconds 300
[UiSmokeNative]::Click(($org.X + $cx), ($org.Y + $cy))
# 「暂停」现在先弹时长菜单（15 分钟 / 30 分钟 / 1 小时 / 2 小时 / 直到手动恢复）：
# 菜单刚弹出时没有选中项，按一次 ↓ 选第一项，回车确认。
Start-Sleep -Milliseconds 400
[UiSmokeNative]::Key(0x28) # VK_DOWN
[UiSmokeNative]::Key(0x0D) # VK_RETURN
$paused = Wait-Status { param($s) $s.paused -eq $true } 15
$stPaused = Get-Status
$timedPause = ($stPaused.pause_until -gt 0) -and ($stPaused.pause_remain_sec -gt 0)
# 撤销接管后要等巡检（1.5s 一轮）把断电自愈项也撤掉——不能只睡固定时长，否则慢一点就误报。
$disarmed = Wait-Until { (Get-RunOnceValue) -eq "" } 6
# 再等窗口把这一帧画完（按钮标签翻成「恢复」）才截图：
# 暂停要还原 hosts，过程中会有一两秒的「正在暂停加速…」过渡帧，截到它看不出暂停后的样子。
$labelPaused = Wait-Until { [string](Get-Region "pause").label -eq "恢复" } 10
Shot $hwnd (Join-Path $ShotDir "02-paused.png") | Out-Null
$pauseLabelPaused = [string](Get-Region "pause").label
$runOncePaused = Get-RunOnceValue
$enablePaused = Get-ProxyEnable
$hostsPaused = Test-HostsHasManagedBlock

if (-not $paused) { $script:failures.Add("点了「暂停」但状态没有变成 paused") }
if (-not $timedPause) { $script:failures.Add("选「暂停 15 分钟」后没有定时：pause_until=$($stPaused.pause_until) pause_remain_sec=$($stPaused.pause_remain_sec)") }
if ($beforeProxy -and $enablePaused -ne 0) { $script:failures.Add("暂停后系统代理没有撤回：ProxyEnable=$enablePaused") }
if ($beforeHosts -and $hostsPaused) { $script:failures.Add("暂停后 hosts 托管块没有移除") }
if (-not $disarmed) { $script:failures.Add("暂停后断电自愈项没有撤掉：'$runOncePaused'") }
if (-not $labelPaused) { $script:failures.Add("暂停后按钮标签没有变成「恢复」（当前 '$pauseLabelPaused'）") }

# —— 5. 恢复：全部回来，自愈项重新登记 ——
Write-Host "[5/10] 点击「恢复」…" -ForegroundColor Gray
[UiSmokeNative]::Force($hwnd)
Start-Sleep -Milliseconds 300
[UiSmokeNative]::Click(($org.X + $cx), ($org.Y + $cy))
$resumed = Wait-Status { param($s) $s.paused -eq $false -and $s.running } 15
$armed = Wait-Until { (Get-RunOnceValue) -eq $runOnceWant } 10
# 等窗口把标签翻回「暂停」再截图（恢复要重新接管 hosts，同样有过渡帧）。
$labelResumed = Wait-Until { [string](Get-Region "pause").label -eq "暂停" } 10
Shot $hwnd (Join-Path $ShotDir "03-resumed.png") | Out-Null
$st = Get-Status
$pauseLabelResumed = [string](Get-Region "pause").label
$enableResumed = Get-ProxyEnable
$hostsResumed = Test-HostsHasManagedBlock

if (-not $resumed) { $script:failures.Add("点了「恢复」但状态没有回到运行中") }
if ($beforeProxy -and -not $st.system_proxy) { $script:failures.Add("恢复后系统代理没有重新接管") }
if ($beforeProxy -and $enableResumed -ne 1) { $script:failures.Add("恢复后 ProxyEnable=$enableResumed，期望 1") }
if ($beforeHosts -and -not $hostsResumed) { $script:failures.Add("恢复后 hosts 托管块没有回来") }
if (-not $armed) { $script:failures.Add("恢复后断电自愈项没有重新登记：'$(Get-RunOnceValue)'") }
if (-not $labelResumed) { $script:failures.Add("恢复后按钮标签没有变回「暂停」（当前 '$pauseLabelResumed'）") }

# —— 6. 日志区右键 → 复制代理地址 ——
Write-Host "[6/10] 日志区右键「复制代理地址」…" -ForegroundColor Gray
Set-Clipboard -Value "tongtu-sentinel"
[UiSmokeNative]::Force($hwnd)
Start-Sleep -Milliseconds 500
[UiSmokeNative]::RightClick(($org.X + 300), ($org.Y + 300))
Start-Sleep -Milliseconds 900
Shot $hwnd (Join-Path $ShotDir "04-context-menu.png") | Out-Null
# 菜单项顺序：复制 / 全选 / --- / 复制代理地址 / 打开配置文件 / 清空日志
[UiSmokeNative]::Key(0x28); [UiSmokeNative]::Key(0x28); [UiSmokeNative]::Key(0x28)
[UiSmokeNative]::Key(0x0D)
Start-Sleep -Milliseconds 800
$clip = Get-Clipboard
if ($clip -ne $proxyURL) { $script:failures.Add("复制代理地址得到 '$clip'，期望 '$proxyURL'") }

# —— 7. 改配置热生效 ——
if ($SkipHotReload) {
    Write-Host "[7/10] 跳过热加载检查（-SkipHotReload）。" -ForegroundColor Gray
} else {
    Write-Host "[7/10] 改配置验证热生效…" -ForegroundColor Gray
    $domain = "smoke{0}.example" -f (Get-Random -Minimum 1000 -Maximum 99999)
    $text = [IO.File]::ReadAllText($cfgPath, [System.Text.Encoding]::UTF8)
    $hitsBefore = @(Select-String -Path $logFile -Pattern "配置已热生效" -ErrorAction SilentlyContinue).Count
    $text = Add-SmokeCustomDomain $text $domain
    [IO.File]::WriteAllText($cfgPath, $text, (New-Object System.Text.UTF8Encoding($false)))
    $script:cfgChanged = $true
    $hot = Wait-Until {
        @(Select-String -Path $logFile -Pattern "配置已热生效" -ErrorAction SilentlyContinue).Count -gt $hitsBefore
    } 15
    Start-Sleep -Milliseconds 600
    Shot $hwnd (Join-Path $ShotDir "05-hot-reload.png") | Out-Null
    if (-not $hot) { $script:failures.Add("改配置后没有出现「配置已热生效」日志") }
    if ($beforeHosts -and -not (Test-HostsHasManagedBlock)) {
        $script:failures.Add("热加载后 hosts 托管块消失了")
    }
}

# —— 8. 诊断面板：打开 → 输入域名 → 诊断 → 等结论 → 关闭 ——
Write-Host "[8/10] 打开诊断面板，诊断 github.com…" -ForegroundColor Gray
$probeOpened = $false
$probeDone = $false
$probeClosed = $false
$probeVerdict = ""
[UiSmokeNative]::Force($hwnd)
Start-Sleep -Milliseconds 300
$probeBtn = Get-Region "probe"
if ($null -eq $probeBtn) {
    $script:failures.Add("界面里没有「诊断」按钮（id=probe）")
} else {
    $px, $py = Get-RegionCenter $probeBtn
    [UiSmokeNative]::Click(($org.X + $px), ($org.Y + $py))
    Start-Sleep -Milliseconds 500
    $probeLayout = Get-UILayout
    $probeOpened = ($null -ne $probeLayout) -and [bool]$probeLayout.probe_open
    if (-not $probeOpened) { $script:failures.Add("点「诊断」后快照没有标记 probe_open") }

    $runBtn = Get-Region "proberun"
    $edit = [UiSmokeNative]::VisibleEdit($hwnd)
    if ($null -eq $runBtn) {
        $script:failures.Add("诊断面板里没有运行按钮（id=proberun）")
    } elseif ($edit -eq [IntPtr]::Zero) {
        $script:failures.Add("诊断面板打开后找不到可见的域名输入框")
    } else {
        [UiSmokeNative]::SetText($edit, "github.com")
        Start-Sleep -Milliseconds 300
        $rx, $ry = Get-RegionCenter $runBtn
        [UiSmokeNative]::Click(($org.X + $rx), ($org.Y + $ry))
        # 结论在状态条上只闪几秒、日志里也没有，因此以 /ui/layout 的 probe_verdict 为准。
        $probeDone = Wait-Until {
            $l = Get-UILayout
            return ($null -ne $l) -and [bool]$l.probe_open -and (-not [bool]$l.probe_busy) -and
                ([string]$l.probe_verdict -ne "")
        } 50
        $shotLayout = Get-UILayout
        if ($null -ne $shotLayout) { $probeVerdict = [string]$shotLayout.probe_verdict }
        Shot $hwnd (Join-Path $ShotDir "06-probe.png") | Out-Null
        if (-not $probeDone) {
            $script:failures.Add("诊断没有在 50 秒内给出结论（probe_verdict='$probeVerdict'）")
        }
    }

    # 再点一次「诊断」回到日志：面板开关必须双向可用，否则用户会被困在面板里。
    $toggle = Get-Region "probe"
    if ($null -ne $toggle) {
        $tx, $ty = Get-RegionCenter $toggle
        [UiSmokeNative]::Click(($org.X + $tx), ($org.Y + $ty))
        Start-Sleep -Milliseconds 500
        $afterClose = Get-UILayout
        $probeClosed = ($null -ne $afterClose) -and (-not [bool]$afterClose.probe_open)
        if (-not $probeClosed) { $script:failures.Add("再点「诊断」后面板没有关闭") }
    }
}

# —— 9. 点 × 只隐藏窗口，进程继续跑 ——
Write-Host "[9/10] 点 ×，确认只隐藏窗口…" -ForegroundColor Gray
[UiSmokeNative]::Force($hwnd)
Start-Sleep -Milliseconds 300
[UiSmokeNative]::PostMessage($hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null
Start-Sleep -Milliseconds 800
$hidden = -not [UiSmokeNative]::IsWindowVisible($hwnd)
$stAlive = Get-Status
if (-not $hidden) { $script:failures.Add("点 × 之后窗口还可见（应该只隐藏）") }
if ($script:proc.HasExited) { $script:failures.Add("点 × 之后进程退出了（应该继续运行）") }
if ($null -eq $stAlive -or -not $stAlive.running) { $script:failures.Add("点 × 之后控制端口不可用了（服务应继续运行）") }

Write-Host "[10/10] 结果" -ForegroundColor Gray
Write-Host ""
$rows = @(
    @{ Name = "暂停生效";       Ok = $paused;                 Detail = "paused=$($null -ne $paused -and $paused)" },
    @{ Name = "暂停定时生效";   Ok = $timedPause;             Detail = "剩余 $($stPaused.pause_remain_sec) 秒" },
    @{ Name = "暂停撤销接管";   Ok = ((-not $beforeProxy -or $enablePaused -eq 0) -and (-not $beforeHosts -or -not $hostsPaused)); Detail = "ProxyEnable=$enablePaused hosts 托管块=$hostsPaused" },
    @{ Name = "暂停撤销自愈项"; Ok = ($runOncePaused -eq "");  Detail = $(if ($runOncePaused) { $runOncePaused } else { "已撤掉" }) },
    @{ Name = "恢复生效";       Ok = $resumed;                Detail = "system_proxy=$($st.system_proxy) hosts=$($st.hosts)" },
    @{ Name = "恢复重新登记";   Ok = $armed;                  Detail = $(if ($armed) { "已登记" } else { "未登记" }) },
    @{ Name = "按钮标签随状态变"; Ok = (($pauseLabelPaused -eq "恢复") -and ($pauseLabelResumed -eq "暂停")); Detail = "暂停后='$pauseLabelPaused' 恢复后='$pauseLabelResumed'" },
    @{ Name = "复制代理地址";   Ok = ($clip -eq $proxyURL);    Detail = "'$clip'" },
    @{ Name = "诊断面板开关";   Ok = ($probeOpened -and $probeClosed); Detail = "打开=$probeOpened 关闭=$probeClosed" },
    @{ Name = "诊断给出结论";   Ok = $probeDone;              Detail = $(if ($probeVerdict) { $probeVerdict } else { "无结论" }) },
    @{ Name = "× 只隐藏窗口";   Ok = ($hidden -and -not $script:proc.HasExited); Detail = "hidden=$hidden 进程存活=$(-not $script:proc.HasExited)" }
)
foreach ($r in $rows) {
    $mark = if ($r.Ok) { "OK  " } else { "FAIL" }
    $color = if ($r.Ok) { "Green" } else { "Red" }
    Write-Host ("  [{0}] {1,-16} {2}" -f $mark, $r.Name, $r.Detail) -ForegroundColor $color
}
Write-Host ""

# —— 收尾：还原配置 / hosts / 系统代理，停掉实例 ——
try {
    if ($script:cfgChanged -and -not $KeepConfig) {
        [IO.File]::WriteAllBytes($cfgPath, $script:cfgBackup)
        Write-Host "已还原 config.yaml。" -ForegroundColor Gray
        Start-Sleep -Seconds 1
    } elseif ($script:cfgChanged) {
        $script:notes.Add("按 -KeepConfig 保留了配置改动（记得手动改回去）")
    }
    Start-Process -FilePath $Exe -ArgumentList 'stop' -Wait | Out-Null
    if (-not (Wait-Until { (Get-TongtuProcesses).Count -eq 0 } 15)) {
        Get-TongtuProcesses | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
        $script:notes.Add("实例没能在 15 秒内正常退出，已强制结束")
    }
    if ($null -ne $script:hostsBackup -and (Test-HostsHasManagedBlock)) {
        try {
            [IO.File]::WriteAllBytes($hostsPath, $script:hostsBackup)
            Write-Host "已还原 hosts。" -ForegroundColor Gray
        } catch {
            $script:notes.Add("hosts 还原失败（需要管理员权限）：$($_.Exception.Message)")
        }
    }
    $curEnable = Get-ProxyEnable
    $curServer = [string](Get-ItemProperty $regPath).ProxyServer
    if ($curEnable -ne $snapEnable -or $curServer -ne $snapServer) {
        New-ItemProperty -Path $regPath -Name "ProxyEnable" -Value $snapEnable -PropertyType DWord -Force | Out-Null
        if ($snapServer) {
            New-ItemProperty -Path $regPath -Name "ProxyServer" -Value $snapServer -PropertyType String -Force | Out-Null
        }
        Write-Host "已还原系统代理。" -ForegroundColor Gray
    }
    if ((Get-RunOnceValue) -ne "") {
        Remove-ItemProperty -Path $runOnceKey -Name $runOnceName -ErrorAction SilentlyContinue
        $script:notes.Add("收尾时发现 RunOnce 项还在，已手动清除")
    }
} finally {
    Write-Host ("收尾: ProxyEnable={0} hosts 托管块={1} 进程={2} RunOnce='{3}'" -f `
        (Get-ProxyEnable), (Test-HostsHasManagedBlock), (Get-TongtuProcesses).Count, (Get-RunOnceValue)) -ForegroundColor Gray
}

foreach ($n in $script:notes) { Write-Host "备注：$n" -ForegroundColor DarkYellow }
if ($script:failures.Count -eq 0) {
    Write-Host "结论：通过。按钮点击、暂停/恢复、右键复制、热加载、诊断面板、× 只隐藏都符合预期。" -ForegroundColor Green
    Write-Host "截图见: $ShotDir" -ForegroundColor Gray
    exit 0
}
Write-Host "结论：未通过。" -ForegroundColor Red
foreach ($f in $script:failures) { Write-Host "      - $f" -ForegroundColor Red }
Write-Host "截图见: $ShotDir" -ForegroundColor Gray
exit 1
