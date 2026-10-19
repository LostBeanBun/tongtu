//go:build windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"tongtu/internal/config"
	"tongtu/internal/icon"
)

// 本文件实现「通途自己的窗口」——双击启动时的界面。
//
// 为什么不直接用控制台窗口：Windows 上点击控制台窗口的 X 时，系统会给所有已连接的
// 进程发 CTRL_CLOSE_EVENT，并在处理函数返回之后强制结束进程（返回「已处理」也拦不住，
// 最多多给 5 秒做清理）。也就是说控制台窗口天生做不到「点 X 只关窗口、不关程序」。
// 因此双击启动的实例改用本文件的窗口承载状态与运行日志：
//   - 点 X（或 Alt+F4）只是隐藏窗口，服务与托盘继续运行；
//   - 再打开：左键点托盘图标，或托盘右键 →「显示/隐藏窗口」。
//
// UI 要点：随系统主题（深/浅色）着色、按显示器 DPI 渲染、顶部状态条随状态刷新、
// 日志区不打断用户划选与往上翻看、窗口位置与大小记忆到 %APPDATA%\tongtu\window.json。

// hasLogWindow 表示当前平台提供独立的日志窗口（供 start.go 决定是否挂托盘菜单项）。
const hasLogWindow = true

// 窗口消息、控件样式与常量（见 MSDN）。
const (
	wmSetRedraw       = 0x000B
	wmGetText         = 0x000D
	wmGetTextLen      = 0x000E
	wmPaint           = 0x000F
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEraseBkgnd      = 0x0014
	wmEndSession      = 0x0016
	wmSize            = 0x0005
	wmGetMinMaxInfo   = 0x0024
	wmSetFont         = 0x0030
	wmGetFont         = 0x0031
	wmDpiChanged      = 0x02E0
	wmContextMenu     = 0x007B
	wmExitSizeMove    = 0x0232
	wmCopy            = 0x0301
	wmPrint           = 0x0317
	wmPrintClient     = 0x0318
	wmCtlColorStatic  = 0x0138
	wmCtlColorEdit    = 0x0133
	wmThemeChanged    = 0x031A
	wmVScroll         = 0x0115
	sbBottom          = 7 // WM_VSCROLL 的 SB_BOTTOM

	emGetSel              = 0x00B0
	emSetSel              = 0x00B1
	emGetLineCount        = 0x00BA
	emReplaceSel          = 0x00C2
	emLineScroll          = 0x00B6
	emScrollCaret         = 0x00B7
	emGetFirstVisibleLine = 0x00CE

	wsOverlappedWindow = 0x00CF0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsVScroll          = 0x00200000
	// 父窗口自绘状态条，必须把子控件挖掉，否则绘制会被子控件覆盖（闪）。
	wsClipChildren = 0x02000000
	wsClipSiblings = 0x04000000

	esMultiline   = 0x0004
	esAutoVScroll = 0x0040
	esAutoHScroll = 0x0080
	esLeft        = 0x0000
	esReadOnly    = 0x0800

	swShow         = 0x5
	swShowMaximize = 0x3
	swpNoZOrder    = 0x0004
	swpNoActivate  = 0x0010

	cwUseDefault = -0x80000000

	fwNormal         = 400
	fwSemiBold       = 600
	defaultCharset   = 1
	cleartypeQuality = 5

	smCXIcon   = 11
	smCYIcon   = 12
	smCXSmIcon = 49
	smCYSmIcon = 50

	monitorDefaultToNull = 0
	logPixelsY           = 90

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	mfString       = 0x0000
	mfSeparator    = 0x0800

	// DrawText 与背景模式
	dtLeft        = 0x0000
	dtRight       = 0x0002
	dtVCenter     = 0x0004
	dtSingleLine  = 0x0020
	dtNoPrefix    = 0x0800
	dtEndEllipsis = 0x8000
	bkTransparent = 1

	// 标题栏明暗（DWMWA_USE_IMMERSIVE_DARK_MODE，新属性号与旧属性号都试一遍）
	dwmwaUseImmersiveDarkMode    = 20
	dwmwaUseImmersiveDarkModeOld = 19
)

// windowMaxBytes 限制日志文本长度：超出后只保留尾部，避免长期运行无限增长。
const windowMaxBytes = 256 << 10

// 日志控件的富文本能力：用 msftedit.dll 的 RICHEDIT50W 才能按级别给文字上色。
// 取不到就退回普通 EDIT（见 ensureRichEdit）——不影响功能，只是没有分级配色。
var (
	richEditOnce  sync.Once
	richEditOK    bool
	msfteditDLL   windows.Handle
	richEditClass = utf16Ptr("RICHEDIT50W")
)

// plainEditClass 是普通单行编辑框的类名（过滤输入框用；富文本能力对它没有意义）。
var plainEditClass = utf16Ptr("EDIT")

// richEditActive 报告日志控件是否具备富文本能力。
func richEditActive() bool { return richEditOK }

// richEditModule 返回 msftedit.dll 的模块句柄（0 表示没加载成功）。
// 句柄被保留下来：一是让「富文本可用」与「动态库已加载」这两个状态可以被核对，
// 二是将来若要显式 FreeLibrary 时不用再找一遍。
func richEditModule() windows.Handle { return msfteditDLL }

// ensureRichEdit 加载 msftedit.dll 并选定控件类名（失败时回退到普通 EDIT，不报错）。
func ensureRichEdit() {
	richEditOnce.Do(func() {
		h, err := windows.LoadLibrary("Msftedit.dll")
		if err != nil {
			richEditClass = utf16Ptr("EDIT")
			return
		}
		msfteditDLL, richEditOK = h, true
	})
}

// charFormat2T 是 CHARFORMAT2W（RichEdit 的字符格式）。只用到颜色与加粗两项，
// 但 RichEdit 4.1+ 要求至少传这个大小，因此按完整结构体声明、由 Go 保证偏移与 C 一致。
type charFormat2T struct {
	CBSize        uint32
	Mask          uint32
	Effects       uint32
	Height        int32
	Offset        int32
	TextColor     uint32
	CharSet       byte
	PitchFamily   byte
	FaceName      [32]uint16
	Weight        uint16
	Spacing       int16
	BackColor     uint32
	Lcid          uint32
	Reserved      uint32
	Style         int16
	Kerning       uint16
	UnderlineType byte
	Animation     byte
	RevAuthor     byte
	Reserved1     byte
}

const (
	cfmBold      = 0x0001
	cfmColor     = 0x40000000
	cfeBold      = 0x0001
	scfSelection = 0x0001
	scfAll       = 0x0004

	emSetCharFormat = 0x0444 // EM_SETCHARFORMAT
	emSetBkColor    = 0x0443 // EM_SETBKGNDCOLOR
	emAutoURLDetect = 0x045A // EM_AUTOURLDETECT
)

// 换行、线条与描边、界面度量
const (
	// 线条与描边
	psSolid     = 0 // CreatePen 的实线样式
	nullBrushId = 5 // GetStockObject(NULL_BRUSH)：只要描边不要填充
	nullPenId   = 8 // GetStockObject(NULL_PEN)：只要填充不要描边

	uiFaceName = "Microsoft YaHei UI"
	uiFontPT   = 10
	uiTitlePT  = 11

	// 顶部状态条与卡片工具条的度量（DIP，绘制与命中判定共用，见 ui_layout_windows.go）
	uiRow1DIP      = 36 // 第一行：小图标 / 标题 / 状态词 / 开关胶囊
	uiRow2DIP      = 26 // 第二行：详情（或轻量提示）/ 动作按钮
	uiHeaderDIP    = uiRow1DIP + uiRow2DIP
	uiToolbarDIP   = 30  // 日志卡片内的工具条：过滤框 + 只看警告
	uiControlDIP   = 22  // 胶囊与按钮的高度
	uiGapDIP       = 8   // 控件之间的水平间距
	uiChipPadDIP   = 10  // 胶囊的左右内边距
	uiBtnPadDIP    = 9   // 幽灵按钮的左右内边距
	uiIconDIP      = 16  // 标题前小图标的边长
	uiIconGapDIP   = 8   // 图标与标题的间距
	uiTitleGapDIP  = 10  // 标题与状态词的间距
	uiDotDIP       = 4   // 胶囊里状态点的半径
	uiFilterDIP    = 170 // 过滤输入框的宽度上限
	uiFilterMinDIP = 80  // 过滤输入框窄于这个宽度就不显示（窗口太窄时）
	uiMinWDIP      = 620 // 窗口最小宽度
	uiMinHDIP      = 320 // 窗口最小高度

	uiPadDIP         = 12 // 客户区四周留白
	uiCardPadDIP     = 8  // 日志卡片的内边距（内容相对卡片内缩的距离）
	uiRadiusDIP      = 10 // 日志卡片圆角半径
	uiGhostRadiusDIP = 6  // 幽灵按钮的圆角半径

	// 轻量提示与过滤防抖
	uiToastMS       = 3200 // 轻量提示显示时长（毫秒）
	uiFilterWaitMS  = 180  // 过滤输入防抖（毫秒）
	uiTimerToast    = 1    // SetTimer 的提示定时器 id
	uiTimerFilter   = 2    // SetTimer 的过滤防抖定时器 id
	uiToastEllipsis = "…"
)

// 窗口类名、窗口标题（常驻，避免被 GC 回收）。
var (
	classNamePtr   = utf16Ptr("TongtuLogWindow")
	windowTitlePtr = utf16Ptr("通途 Tongtu — 运行日志")
)

// gdi32 提供字体与画刷相关 API（HICON/画刷/字体都在 gdi32）。
var gdi32 = windows.NewLazySystemDLL("gdi32.dll")

var (
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procSendMessageW                  = user32.NewProc("SendMessageW")
	procIsWindowVisible               = user32.NewProc("IsWindowVisible")
	procGetClientRect                 = user32.NewProc("GetClientRect")
	procMoveWindow                    = user32.NewProc("MoveWindow")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procGetWindowPlacement            = user32.NewProc("GetWindowPlacement")
	procLoadCursorW                   = user32.NewProc("LoadCursorW")
	procSetWindowTextW                = user32.NewProc("SetWindowTextW")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procCreateIconFromResourceEx      = user32.NewProc("CreateIconFromResourceEx")
	procMonitorFromRect               = user32.NewProc("MonitorFromRect")
	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procInvalidateRect                = user32.NewProc("InvalidateRect")
	procUpdateWindow                  = user32.NewProc("UpdateWindow")
	procGetCursorPos                  = user32.NewProc("GetCursorPos")
	procCreatePopupMenu               = user32.NewProc("CreatePopupMenu")
	procDestroyMenu                   = user32.NewProc("DestroyMenu")
	procTrackPopupMenu                = user32.NewProc("TrackPopupMenu")
	procAppendMenuW                   = user32.NewProc("AppendMenuW")

	procGetStockObject   = gdi32.NewProc("GetStockObject")
	procCreatePen        = gdi32.NewProc("CreatePen")
	procRoundRect        = gdi32.NewProc("RoundRect")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procSelectObject     = gdi32.NewProc("SelectObject")
	procGetTextMetricsW  = gdi32.NewProc("GetTextMetricsW")
	procSetTextColor     = gdi32.NewProc("SetTextColor")
	procSetBkColor       = gdi32.NewProc("SetBkColor")
	procSetBkMode        = gdi32.NewProc("SetBkMode")
	procGetDeviceCaps    = gdi32.NewProc("GetDeviceCaps")
	procEllipse          = gdi32.NewProc("Ellipse")
	procTextExtent       = gdi32.NewProc("GetTextExtentPoint32W")

	procBeginPaint = user32.NewProc("BeginPaint")
	procEndPaint   = user32.NewProc("EndPaint")
	procFillRect   = user32.NewProc("FillRect")
	procDrawTextW  = user32.NewProc("DrawTextW")

	procShcoreDpiAwareness    = windows.NewLazySystemDLL("shcore.dll").NewProc("SetProcessDpiAwareness")
	procDwmSetWindowAttr      = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	procUxthemeSetWindowTheme = windows.NewLazySystemDLL("uxtheme.dll").NewProc("SetWindowTheme")
)

// —— 以下 Win32 结构体 x/sys/windows 未提供，按 MSDN 定义 ——

type wndClassExT struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type pointT struct{ X, Y int32 }

// msgT 对应 MSG（x64 下 48 字节）。
type msgT struct {
	Wnd     windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      pointT
	Private uint32
}

type rectT struct{ Left, Top, Right, Bottom int32 }

// paintStructT 对应 PAINTSTRUCT（BeginPaint/EndPaint 需要）。
type paintStructT struct {
	Hdc       windows.Handle
	Erase     int32
	Paint     rectT
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

type minMaxInfoT struct {
	Reserved     pointT
	MaxSize      pointT
	MaxPosition  pointT
	MinTrackSize pointT
	MaxTrackSize pointT
}

// windowPlacementT 对应 WINDOWPLACEMENT（取位置、尺寸与最大化状态）。
type windowPlacementT struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    pointT
	MaxPosition    pointT
	NormalPosition rectT
}

// windowPos 是窗口初始位置与大小。
// 注意不要用 rect 的 Right-Left 计算宽高：默认位置用的是 CW_USEDEFAULT
// （一个很大的负数），相减会溢出 int32，窗口就会退化成最小尺寸。
type windowPos struct{ X, Y, W, H int32 }

type windowState struct {
	X         int32 `json:"x"`
	Y         int32 `json:"y"`
	W         int32 `json:"w"`
	H         int32 `json:"h"`
	Maximized bool  `json:"maximized"`
}

// —— 主题 ——

// uiTheme 是一套配色。
//
// 颜色为 COLORREF（0x00BBGGRR）；画刷/画笔在窗口创建时按主题生成。
//
// 设计规则（保证「色调搭配合理」且可度量，见 TestPaletteContrast / TestPaletteHueHarmony）：
//
//  1. 中性色（表面、线条、三级文字）全部取同一色相（冷灰 H≈240°）：深浅两套只改明度、
//     不改色相，因此看起来是同一个设计的两种明度，而不是两种风格。
//  2. 语义色（强调蓝 / 正常绿 / 降级橙）深浅两套同样共用色相，只在明度上分层——
//     浅色压深一档（压在亮底上足黑），深色用「提升层」高亮度变体（压在暗底上足亮）。
//  3. 每个颜色职责唯一：状态点只负责「开关状态」，文字只用文字色阶（一/二/三级），
//     强调色只给标题。避免同一个颜色既表状态又表强调的配色噪音。
//  4. 对比度按 WCAG 校准：正文 ≥7:1，次要文字 ≥4.5:1，弱化文字与状态点（图形）≥3:1。
type uiTheme struct {
	dark bool

	// 表面与线条
	bg       uint32 // 窗口底色（系统模糊不可用时的回退填充）
	cardBG   uint32 // 日志卡片底色
	hairline uint32 // 发丝线：状态条分隔线、卡片描边

	// 文字三级（对比度依次递减）
	text  uint32 // 一级：日志正文、已开启的开关名
	muted uint32 // 二级：运行状态词、右侧详情
	faint uint32 // 三级：未开启的开关名（弱化但仍可读）

	// 语义色
	accent uint32 // 品牌色：标题（systemBlue）
	ok     uint32 // 正常：状态点、状态词（systemGreen）
	warn   uint32 // 降级：有开关没生效、警告级日志（systemOrange）
	danger uint32 // 失败：错误级日志（systemRed）
	off    uint32 // 关闭：状态点（中性灰）

	// 交互反馈：中性叠加色，由底色与文字色按比例混合而来（见 resolveTheme）。
	// 保持中性灰、不引入新色相，因此不会和语义色抢「谁在表达状态」。
	//
	// 注意：常态**不铺底**（控件平时只有文字与状态点），底色只在悬停/按下/选中时出现。
	// 这样状态点始终落在窗口底色上，对比度不受交互底色影响。
	hover uint32 // 悬停 / 按下底色（胶囊与幽灵按钮共用）
	input uint32 // 输入框（过滤框）的常态底色

	// GDI 对象（每个主题一套：切换主题时整套替换，不新建也不删除）
	bgBrush       uintptr // bg
	cardBrush     uintptr // cardBG
	okBrush       uintptr // ok
	offBrush      uintptr // off
	hoverBrush    uintptr // hover
	inputBrush    uintptr // input
	hairlineBrush uintptr // hairline（分隔线用 FillRect）
	hairlinePen   uintptr // hairline（描边用 RoundRect）
	nullPen       uintptr // 无描边（胶囊填充用）
}

func rgb(r, g, b uint8) uint32 {
	return uint32(r) | uint32(g)<<8 | uint32(b)<<16
}

// stateColor 返回运行状态词该用的颜色：正常用绿、降级用橙、未运行用次要灰。
// 状态词本身就写明了「运行中 / 部分生效 / 已停止」，颜色只是强化，不依赖颜色单通道。
func (t uiTheme) stateColor(l windowStateLevel) uint32 {
	switch l {
	case stateOK:
		return t.ok
	case stateDegraded:
		return t.warn
	default:
		return t.muted
	}
}

// styleColor 把日志显示样式映射到当前主题的颜色（见 logview.go 的 logStyle）。
//
// 与状态条同一套语义：绿=正常/已生效、橙=降级或警告、红=失败、蓝=标题、
// 三级文字色阶负责内容层次——同一个颜色在界面里只表达一件事。
func (t uiTheme) styleColor(s logStyle) uint32 {
	switch s {
	case styleMuted:
		return t.muted
	case styleFaint:
		return t.faint
	case styleAccent:
		return t.accent
	case styleOK:
		return t.ok
	case styleWarn:
		return t.warn
	case styleDanger:
		return t.danger
	default:
		return t.text
	}
}

// 配色取 iOS 系统色：深浅各一套、共用色相、只分亮度；界面是常规窗口——
// 不透明底色 + 圆角卡片 + 一条发丝分隔线，靠配色层次而不是特效来区分区域。
var (
	themeDark = resolveTheme(uiTheme{
		dark:     true,
		bg:       rgb(28, 28, 30),    // #1C1C1E systemBackground
		cardBG:   rgb(44, 44, 46),    // #2C2C2E secondarySystemBackground
		hairline: rgb(58, 58, 60),    // #3A3A3C separator
		text:     rgb(242, 242, 247), // #F2F2F7 label（对卡片 12.5:1）
		muted:    rgb(174, 174, 178), // #AEAEB2 secondaryLabel（6.3:1）
		faint:    rgb(152, 152, 157), // #98989D（4.9:1）
		accent:   rgb(64, 156, 255),  // #409CFF 深色「提升层」systemBlue（4.9:1）
		ok:       rgb(48, 209, 88),   // #30D158 systemGreen（6.9:1）
		warn:     rgb(255, 159, 10),  // #FF9F0A systemOrange
		danger:   rgb(255, 107, 97),  // #FF6B61 同色相 H≈4°（对卡片 4.9:1）
		off:      rgb(142, 142, 147), // #8E8E93（4.3:1）
	})
	themeLight = resolveTheme(uiTheme{
		dark:     false,
		bg:       rgb(242, 242, 247), // #F2F2F7
		cardBG:   rgb(255, 255, 255), // #FFFFFF
		hairline: rgb(209, 209, 214), // #D1D1D6
		text:     rgb(28, 28, 30),    // #1C1C1E（对卡片 17.0:1）
		muted:    rgb(99, 99, 102),   // #636366（6.0:1）
		faint:    rgb(124, 124, 128), // #7C7C80（4.2:1）
		accent:   rgb(10, 111, 216),  // #0A6FD8（同色相 H=211°，4.9:1）
		ok:       rgb(36, 138, 61),   // #248A3D（同色相 H=135°，4.4:1）
		warn:     rgb(168, 90, 0),    // #A85A00（同色相 H≈32°）
		danger:   rgb(196, 43, 31),   // #C42B1F（同色相 H≈4°，对窗口底 5.1:1）
		off:      rgb(124, 124, 128), // #7C7C80（4.2:1）
	})
)

// 交互反馈色的混合比例（相对底色）：深色主题要略强一点才看得出层次。
//
// 悬停色同时用于「按下」与「选中」：按下只多一瞬，选中靠文字色（正文色）区分，
// 因此不必再压深一档——压得太深会让状态点与文字的可读性变差。
const (
	hoverAlphaDark  = 0.16
	hoverAlphaLight = 0.10
	inputAlphaDark  = 0.10
	inputAlphaLight = 0.05
)

// resolveTheme 补齐由底色与文字色推导出来的中性色（悬停底色 / 输入框底色）。
//
// 为什么推导而不是再挑几个颜色：这几个色只承担「这里可以点 / 现在被按住」，
// 与色相无关；从底色往文字色混一步，天然保证是同一个中性灰阶梯。
func resolveTheme(t uiTheme) uiTheme {
	hoverA, inputA := hoverAlphaLight, inputAlphaLight
	if t.dark {
		hoverA, inputA = hoverAlphaDark, inputAlphaDark
	}
	t.hover = blend(t.bg, t.text, hoverA)
	t.input = blend(t.cardBG, t.text, inputA)
	return t
}

// blend 把 src 按 alpha（0~1）混到 dst 上，返回新的 COLORREF。
func blend(dst, src uint32, alpha float64) uint32 {
	mix := func(shift uint) uint32 {
		d := float64((dst >> shift) & 0xFF)
		s := float64((src >> shift) & 0xFF)
		v := d + (s-d)*alpha
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		return uint32(v+0.5) << shift
	}
	return mix(0) | mix(8) | mix(16)
}

// systemThemeDark 读取系统的「应用模式」：深色返回 true。
func systemThemeDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		return false
	}
	return v == 0
}

// —— 通用小工具 ——

func utf16Ptr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

// setDPIAware 声明进程 DPI 感知，避免高分屏下字体发虚。必须在创建窗口之前调用。
// 依次尝试 per-monitor v2 → per-monitor → system aware，都不支持时保持不变。
func setDPIAware() {
	const (
		perMonitorV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
		perMonitor   = 2           // PROCESS_PER_MONITOR_DPI_AWARE
	)
	if procSetProcessDpiAwarenessContext.Find() == nil {
		if r, _, _ := procSetProcessDpiAwarenessContext.Call(perMonitorV2); r != 0 {
			return
		}
	}
	if procShcoreDpiAwareness.Find() == nil {
		if _, _, err := procShcoreDpiAwareness.Call(perMonitor); err == nil {
			return
		}
	}
	if procSetProcessDPIAware.Find() == nil {
		procSetProcessDPIAware.Call()
	}
}

// windowDPI 返回窗口所在显示器的 DPI（取不到时按 96）。
func windowDPI(hwnd windows.Handle) uint32 {
	if procGetDpiForWindow.Find() == nil && hwnd != 0 {
		if r, _, _ := procGetDpiForWindow.Call(uintptr(hwnd)); r >= 72 {
			return uint32(r)
		}
	}
	// 窗口还没创建时用系统 DPI（主显示器）
	if procGetDpiForSystem.Find() == nil {
		if r, _, _ := procGetDpiForSystem.Call(); r >= 72 {
			return uint32(r)
		}
	}
	if hdc, _, _ := procGetDC.Call(0); hdc != 0 {
		r, _, _ := procGetDeviceCaps.Call(hdc, logPixelsY)
		procReleaseDC.Call(0, hdc)
		if r >= 72 {
			return uint32(r)
		}
	}
	return 96
}

func scaleDPI(v int32, dpi uint32) int32 {
	return v * int32(dpi) / 96
}

// —— 日志窗口 ——

// windowStatus 是顶部状态条要展示的内容。
//
// 状态条由窗口自己绘制（不是 STATIC 控件）：状态点与状态词要按健康度变色，
// 自绘才能精确控制颜色与对齐。
type logWindow struct {
	once  sync.Once
	ready chan struct{}

	mu        sync.Mutex
	lines     []logLine // 完整日志（带样式）：窗口创建前先攒着，创建后按样式回填
	lineBytes int       // lines 的字节数，用于限制总长度
	status    windowStatus
	hwnd      windows.Handle
	edit      windows.Handle // 日志控件
	filter    windows.Handle // 过滤输入框
	tooltip   windows.Handle // 悬浮提示
	icon      windows.Handle // 标题前的小图标
	uiLayout  uiLayout       // 当前几何（绘制与命中共用同一份）
	layoutW   int32          // 几何对应的客户区尺寸 / 状态项 / DPI
	layoutH   int32
	layoutKey string
	onlyWarn  bool    // 「只看警告」是否开启
	toastMsg  uiToast // 当前轻量反馈（零值 = 没有）
	// pendingMode 是正在后台切换的开关 id（空 = 没有）。
	// 切换在别的 goroutine 上做（见 toggleMode），窗口线程只负责给反馈。
	pendingMode string
	font        uintptr
	fontTitle   uintptr
	theme       uiTheme
	// 深/浅两套配色在创建窗口时就把画刷准备好：切换主题时整套替换，
	// 不新建也不删除 GDI 对象（因此反复切换也不会泄漏）。
	themeDarkSet  uiTheme
	themeLightSet uiTheme
	dpi           uint32
	wantMax       bool
	err           error

	// 仅窗口线程访问：悬停/按下状态与整帧双缓冲用的 GDI 对象
	hoverID string
	pressID string
	memDC   uintptr
	memBmp  uintptr
	memW    int32
	memH    int32
}

// uiToast 是一条轻量反馈：显示在状态条第二行，几秒后自动消失。
type uiToast struct {
	Text  string
	Style logStyle
	Until time.Time
}

var lw = &logWindow{ready: make(chan struct{})}

// logDisplay 返回日志窗口的显示层（没有界面的平台返回 nil，见 window_other.go）。
func logDisplay() logSink { return lw }

// Write 实现 io.Writer：把裸文本按行塞进日志窗口。
// 日志与启动摘要现在都走带样式的结构化入口（windowHandler / addSummary），
// 这里只作为兜底通道保留。
func (w *logWindow) Write(p []byte) (int, error) {
	text := strings.ReplaceAll(string(p), "\r\n", "\n")
	var lines []logLine
	for _, s := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		lines = append(lines, logLine{Spans: []logSpan{{Style: styleText, Text: s}}})
	}
	w.addLines(lines)
	return len(p), nil
}

// addSummary 追加一段带样式的启动摘要：前后各留一个空行，把摘要和日志流分开。
func (w *logWindow) addSummary(title string, rows []summaryRow) {
	lines := renderSummary(title, rows)
	if len(lines) == 0 {
		return
	}
	w.addLines(append([]logLine{{}}, lines...))
}

// addLines 追加显示行，并维持总长度上限（超出后从头丢，按整行丢弃）。
// 窗口还没创建时只攒在缓冲里，创建后由 renderAll 按样式回填。
func (w *logWindow) addLines(lines []logLine) {
	if len(lines) == 0 {
		return
	}
	w.mu.Lock()
	for _, l := range lines {
		w.lines = append(w.lines, l)
		w.lineBytes += l.size()
	}
	cut := 0
	for w.lineBytes > windowMaxBytes && cut < len(w.lines) {
		w.lineBytes -= w.lines[cut].size()
		cut++
	}
	if cut > 0 {
		rest := make([]logLine, len(w.lines)-cut)
		copy(rest, w.lines[cut:])
		w.lines = rest
	}
	edit, theme := w.edit, w.theme
	w.mu.Unlock()

	if edit == 0 {
		return // 窗口还没起来：内容已在缓冲里，创建后回填
	}
	if w.filterActive() {
		// 过滤生效中：新日志不一定满足条件，不能直接追加——请求一次（防抖的）整体重画
		w.scheduleFilter()
		return
	}
	if cut > 0 {
		w.renderAll() // 裁剪过：整体重画，样式才不会错位
		return
	}
	appendLines(edit, theme, lines)
}

// renderAll 用缓冲区里的行整体重画日志控件（窗口创建回填 / 裁剪后 / 主题切换换色）。
func (w *logWindow) renderAll() {
	w.mu.Lock()
	lines := make([]logLine, len(w.lines))
	copy(lines, w.lines)
	w.mu.Unlock()
	w.renderLines(lines)
}

// renderLines 用给定的行整体重画日志控件（过滤后的子集也走这里）。
func (w *logWindow) renderLines(lines []logLine) {
	w.mu.Lock()
	edit, theme := w.edit, w.theme
	w.mu.Unlock()
	if edit == 0 || theme.cardBrush == 0 {
		return
	}
	setEditText(edit, "") // 清空（连同旧样式）
	initEditColors(edit, theme)
	appendLines(edit, theme, lines)
	scrollEditToBottom(edit)
}

// appendLines 把行追加到日志控件末尾，同时尽量不打断用户：
//   - 正在划选文本（准备复制）时保留选区；
//   - 已经往上翻看历史时保持当前视野，不把视图拽回末尾；
//   - 停在末尾时跟随最新日志（像 tail -f）。
func appendLines(edit windows.Handle, theme uiTheme, lines []logLine) {
	if edit == 0 || len(lines) == 0 {
		return
	}
	var selStart, selEnd uint32
	procSendMessageW.Call(uintptr(edit), emGetSel,
		uintptr(unsafe.Pointer(&selStart)), uintptr(unsafe.Pointer(&selEnd)))
	hadSel := selStart != selEnd
	follow := !hadSel && editAtBottom(edit)
	first := editFirstVisibleLine(edit)

	procSendMessageW.Call(uintptr(edit), wmSetRedraw, 0, 0)
	for _, l := range lines {
		insertLine(edit, theme, l)
	}
	procSendMessageW.Call(uintptr(edit), wmSetRedraw, 1, 0)

	// 滚动放在「恢复重画」之后。
	//
	// 为什么：WM_SETREDRAW(FALSE) 期间控件只会把滚动「记账」、不重画，恢复重画后它再
	// 用 ScrollWindow 搬动屏幕像素——被露出来的那一条没人重画，于是旧的文字会和新画的
	// 文字重叠（日志刷得快时肉眼可见）。放在恢复重画之后，控件自己会把露出的部分补上。
	if hadSel {
		procSendMessageW.Call(uintptr(edit), emSetSel, uintptr(selStart), uintptr(selEnd))
	} else if follow {
		scrollEditToBottom(edit)
	} else if d := first - editFirstVisibleLine(edit); d != 0 {
		procSendMessageW.Call(uintptr(edit), emLineScroll, 0, uintptr(d))
	}

	procInvalidateRect.Call(uintptr(edit), 0, 1) // 1 = bErase：字数变化后整块重画
	// 再立刻把这一帧画出来（UpdateWindow 直接触发 WM_PAINT，不排队等下一次消息循环），
	// 保证本次追加在返回前就已经完整落到屏幕上。
	procUpdateWindow.Call(uintptr(edit))
}

// insertLine 把一行追加到末尾：逐段上色（RichEdit），再补上换行。
func insertLine(edit windows.Handle, theme uiTheme, l logLine) {
	if edit == 0 {
		return
	}
	n, _, _ := procSendMessageW.Call(uintptr(edit), wmGetTextLen, 0, 0)
	procSendMessageW.Call(uintptr(edit), emSetSel, n, n) // 插入点移到末尾
	for _, sp := range l.Spans {
		setEditCharFormat(edit, theme.styleColor(sp.Style), sp.Bold)
		insertEditText(edit, sp.Text)
	}
	setEditCharFormat(edit, theme.styleColor(styleText), false) // 行尾回到正文色
	insertEditText(edit, "\r\n")
}

func insertEditText(edit windows.Handle, text string) {
	if edit == 0 || text == "" {
		return
	}
	if p := utf16Ptr(text); p != nil {
		procSendMessageW.Call(uintptr(edit), emReplaceSel, 0, uintptr(unsafe.Pointer(p)))
	}
}

// setEditCharFormat 设定「接下来插入的文本」的颜色与粗细（仅 RichEdit 有效）。
func setEditCharFormat(edit windows.Handle, color uint32, bold bool) {
	if edit == 0 || !richEditActive() {
		return
	}
	cf := charFormat2T{
		CBSize:    uint32(unsafe.Sizeof(charFormat2T{})),
		Mask:      cfmColor,
		TextColor: color,
	}
	if bold {
		cf.Mask |= cfmBold
		cf.Effects = cfeBold
	}
	procSendMessageW.Call(uintptr(edit), emSetCharFormat, scfSelection, uintptr(unsafe.Pointer(&cf)))
}

// initEditColors 设定日志控件的底色与默认字色（RichEdit 需要显式设置，否则是白底黑字）。
func initEditColors(edit windows.Handle, theme uiTheme) {
	if edit == 0 || !richEditActive() {
		return
	}
	procSendMessageW.Call(uintptr(edit), emSetBkColor, 0, uintptr(theme.cardBG))
	cf := charFormat2T{
		CBSize:    uint32(unsafe.Sizeof(charFormat2T{})),
		Mask:      cfmColor,
		TextColor: theme.text,
	}
	procSendMessageW.Call(uintptr(edit), emSetCharFormat, scfAll, uintptr(unsafe.Pointer(&cf)))
	// 关掉「自动把 URL 变成蓝色下划线链接」：日志里的链接保持正文色，免得和配色打架
	procSendMessageW.Call(uintptr(edit), emAutoURLDetect, 0, 0)
}

// setStatus 更新顶部状态条（窗口未就绪时先记下，创建后回填）。
func (w *logWindow) setStatus(st windowStatus) {
	w.mu.Lock()
	if w.status.key() == st.key() {
		w.mu.Unlock()
		return
	}
	w.status = st
	hwnd := w.hwnd
	w.mu.Unlock()
	if hwnd != 0 {
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
	}
}

// statusSnapshot 取当前状态条内容的副本。
func (w *logWindow) statusSnapshot() windowStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *logWindow) handle() windows.Handle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hwnd
}

func (w *logWindow) editHandle() windows.Handle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.edit
}

// ensure 创建窗口（只尝试一次）并等待其就绪。
func (w *logWindow) ensure() error {
	w.once.Do(func() { go w.loop() })
	<-w.ready
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// show 显示窗口（必要时创建），并把它带到前台。
func (w *logWindow) show() error {
	if err := w.ensure(); err != nil {
		return err
	}
	h := w.handle()
	if h == 0 {
		return fmt.Errorf("日志窗口不可用")
	}
	w.mu.Lock()
	wantMax := w.wantMax
	w.mu.Unlock()
	cmd := uintptr(swShow)
	if wantMax {
		cmd = swShowMaximize
	}
	procShowWindow.Call(uintptr(h), cmd)
	procSetForegroundWindow.Call(uintptr(h))
	return nil
}

// hide 隐藏窗口（窗口尚未创建时是空操作）。
func (w *logWindow) hide() {
	if h := w.handle(); h != 0 {
		procShowWindow.Call(uintptr(h), swHide)
	}
}

func (w *logWindow) visible() bool {
	h := w.handle()
	if h == 0 {
		return false
	}
	r, _, _ := procIsWindowVisible.Call(uintptr(h))
	return r != 0
}

// toggle 显示/隐藏窗口。
func (w *logWindow) toggle() error {
	if w.visible() {
		w.hide()
		return nil
	}
	return w.show()
}

// loop 是窗口线程：创建窗口 + 跑消息循环。
// 消息循环必须始终在同一个 OS 线程上，因此用 LockOSThread 钉住。
func (w *logWindow) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hwnd, edit, err := w.create()

	w.mu.Lock()
	w.hwnd, w.edit, w.err = hwnd, edit, err
	dpi := w.dpi
	w.mu.Unlock()

	if err == nil {
		// 字体与布局依赖控件句柄，必须在句柄落到结构体之后再套用
		w.applyFont(dpi)
		w.layout()
		w.renderAll() // 回填窗口创建前产生的日志（按样式重画）
	}
	close(w.ready)

	if err != nil {
		return
	}

	var msg msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT，-1 = 出错
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (w *logWindow) create() (windows.Handle, windows.Handle, error) {
	// GetModuleHandleW(NULL) → 本进程 exe 的模块句柄（注册窗口类需要）
	hInst, _, callErr := procGetModuleHandleW.Call(0)
	if hInst == 0 {
		return 0, 0, fmt.Errorf("获取模块句柄失败: %v", callErr)
	}

	// 深浅两套配色的画刷都在这里建好：切换系统主题时整套替换，见 applySystemTheme
	darkSet, lightSet := buildBrushes(themeDark), buildBrushes(themeLight)
	theme := lightSet
	if systemThemeDark() {
		theme = darkSet
	}

	dpi := windowDPI(0)
	cursor, _, _ := procLoadCursorW.Call(0, 32512) // IDC_ARROW
	bigIcon := iconHandle(icon.ICO, int32(procGetSystemMetricsInt(smCXIcon)), int32(procGetSystemMetricsInt(smCYIcon)), dpi)
	smallIcon := iconHandle(icon.ICO, int32(procGetSystemMetricsInt(smCXSmIcon)), int32(procGetSystemMetricsInt(smCYSmIcon)), dpi)

	cls := wndClassExT{
		WndProc:  syscall.NewCallback(wndProc),
		Instance: windows.Handle(hInst),
		Icon:     bigIcon,
		Cursor:   windows.Handle(cursor),
		// 背景画刷留空：客户区由 WM_PAINT 自己画（顶部状态条要能半透明地压在
		// 模糊材质上），同时避免系统先擦一遍背景带来的闪烁。
		Background: 0,
		ClassName:  classNamePtr,
		IconSm:     smallIcon,
	}
	cls.Size = uint32(unsafe.Sizeof(cls))
	if r, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls))); r == 0 {
		return 0, 0, fmt.Errorf("注册窗口类失败: %v", callErr)
	}

	// 位置与大小：优先用上次记住的（并确认还落在某个显示器上）
	pos, wantMax := savedWindowPos(dpi)
	if wantMax {
		w.mu.Lock()
		w.wantMax = true
		w.mu.Unlock()
	}

	// 注意参数顺序：CreateWindowExW(exStyle, 类名, 窗口标题, style, ...)
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(windowTitlePtr)),
		wsOverlappedWindow|wsClipChildren|wsClipSiblings,
		uintptr(uint32(pos.X)), uintptr(uint32(pos.Y)),
		uintptr(uint32(pos.W)), uintptr(uint32(pos.H)),
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return 0, 0, fmt.Errorf("创建窗口失败: %v", callErr)
	}

	// 日志卡片：只读多行编辑框，负责划选/复制/滚动等交互（保留系统行为）。
	// 优先用 msftedit 的富文本控件（可按级别上色），拿不到就退回普通 EDIT。
	ensureRichEdit()
	edit, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(richEditClass)),
		0,
		wsChild|wsVisible|wsVScroll|esMultiline|esAutoVScroll|esReadOnly,
		0, 0, 100, 100,
		hwnd, 0, hInst, 0,
	)
	if edit == 0 {
		return 0, 0, fmt.Errorf("创建日志控件失败: %v", callErr)
	}

	// 过滤输入框：单行 EDIT，摆在卡片工具条左侧（几何见 computeUILayout）。
	filter, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(plainEditClass)),
		0,
		wsChild|wsVisible|esLeft|esAutoHScroll,
		0, 0, 10, 10,
		hwnd, 0, hInst, 0,
	)
	if filter == 0 {
		return 0, 0, fmt.Errorf("创建过滤输入框失败: %v", callErr)
	}
	if cue := utf16Ptr("过滤日志…"); cue != nil {
		procSendMessageW.Call(uintptr(filter), emSetCueBanner, ecbTrue, uintptr(unsafe.Pointer(cue)))
	}

	w.mu.Lock()
	w.theme, w.themeDarkSet, w.themeLightSet, w.dpi = theme, darkSet, lightSet, dpi
	w.filter, w.icon = windows.Handle(filter), smallIcon
	w.mu.Unlock()

	// 常规窗口样式：只把标题栏的明暗交给系统（跟随系统深浅色设置）。
	applyWindowChrome(windows.Handle(hwnd), theme)

	applyEditTheme(windows.Handle(edit), theme.dark)
	initEditColors(windows.Handle(edit), theme)

	// 悬浮提示：贴在窗口上，鼠标移到胶囊/按钮上时显示说明（建不出来也不影响其他功能）
	w.createTooltip(windows.Handle(hwnd))

	return windows.Handle(hwnd), windows.Handle(edit), nil
}

// applyEditTheme 让日志卡片的滚动条跟随深浅色（uxtheme 的两套现成类名）。
func applyEditTheme(edit windows.Handle, dark bool) {
	if edit == 0 {
		return
	}
	name := "Explorer"
	if dark {
		name = "DarkMode_Explorer"
	}
	setWindowTheme(edit, name)
}

// newSolidBrush 创建一个实心画刷（hollow 表示创建失败）。
func newSolidBrush(color uint32) uintptr {
	b, _, _ := procCreateSolidBrush.Call(uintptr(color))
	return b
}

// buildBrushes 复制一份配色并备好 GDI 对象（颜色是纯数值，只有画刷/画笔需要创建）。
//
// 发丝线用 1 物理像素：不随 DPI 放大（高 DPI 下细线更精致），因此不需要随 DPI 重建。
func buildBrushes(t uiTheme) uiTheme {
	t.bgBrush = newSolidBrush(t.bg)
	t.cardBrush = newSolidBrush(t.cardBG)
	t.okBrush = newSolidBrush(t.ok)
	t.offBrush = newSolidBrush(t.off)
	t.hoverBrush = newSolidBrush(t.hover)
	t.inputBrush = newSolidBrush(t.input)
	t.hairlineBrush = newSolidBrush(t.hairline)
	t.hairlinePen = newPen(t.hairline)
	// NULL_PEN：胶囊/按钮只要填充不要描边（RoundRect 必须选入一支笔）
	t.nullPen, _, _ = procGetStockObject.Call(nullPenId)
	return t
}

// newPen 创建 1 像素实线画笔，失败返回 0。
func newPen(color uint32) uintptr {
	pen, _, _ := procCreatePen.Call(psSolid, 1, uintptr(color))
	return pen
}

// applySystemTheme 在系统于浅色/深色之间切换时（WM_THEMECHANGED）重建界面外观：
// 换整套配色与画刷（不新建、不删除任何 GDI 对象）＋ 标题栏与日志卡片主题 ＋
// 让系统绘制的菜单（托盘右键、窗口右键）立刻换肤，然后重绘。无需重启通途。
func (w *logWindow) applySystemTheme(hwnd windows.Handle) {
	w.mu.Lock()
	theme, edit := w.themeLightSet, w.edit
	if systemThemeDark() {
		theme = w.themeDarkSet
	}
	if theme.bgBrush == 0 && theme.cardBrush == 0 {
		w.mu.Unlock()
		return // 画刷还没建好（窗口创建中）：保持现状
	}
	w.theme = theme
	w.mu.Unlock()

	// 常规窗口样式：标题栏的明暗跟着主题走。
	applyWindowChrome(hwnd, theme)
	applyEditTheme(windows.Handle(edit), theme.dark)
	enableDarkMenus() // FlushMenuThemes：菜单立即换肤

	w.layout()
	procInvalidateRect.Call(uintptr(hwnd), 0, 1)
	if edit != 0 {
		// 日志是逐段上色的：换主题要整体重画，颜色才会跟着换（不是只刷一次背景）
		initEditColors(windows.Handle(edit), theme)
		w.renderAll()
	}
	if filter := w.filterHandle(); filter != 0 {
		procInvalidateRect.Call(uintptr(filter), 0, 1) // 过滤框底色跟着主题走
	}
}

// applyWindowChrome 套用常规窗口样式：只把标题栏的明暗交给 DWM，让窗口跟随系统的深浅色。
//
// 这里刻意**不做**「液态玻璃」/系统背景材质，原因（真机实测，Windows 11 24H2 + GDI 自绘）：
//
//   - DwmExtendFrameIntoClientArea(-1) 会把整块客户区变成 DWM 的「玻璃」，我们 GDI 画上去的
//     像素会被按材质的不透明度重新合成——实色被冲淡到 60%~80%，深色主题下浅色文字直接糊到
//     看不见；壁纸偏亮时材质本身还会发白，等于给窗口蒙一层白纱。
//   - 不伸框只设 DWMWA_SYSTEMBACKDROP_TYPE（云母）虽然客户区安全，但也只是标题栏多一层磨砂，
//     与「常规窗口」的观感差别很小。
//   - 未公开的 SetWindowCompositionAttribute（亚克力）在本机（以及新款 Windows 上）返回成功
//     却不生效，同样会把客户区冲淡。
//
// 结论：GDI 自绘的窗口就老老实实用系统默认外观——标题栏跟随主题、圆角交给系统默认处理，
// 客户区由我们铺不透明底色，颜色才落得准、文字才够清楚。
func applyWindowChrome(hwnd windows.Handle, theme uiTheme) {
	setTitleBarTheme(hwnd, theme.dark)
}

// applyFont 按 DPI 创建 UI 字体并套用到日志控件。
// 状态条是自绘的，因此两份字体（正文 / 标题）都要留在结构体里供绘制使用。
func (w *logWindow) applyFont(dpi uint32) {
	body := createUIFont(uiFontPT, fwNormal, dpi)
	title := createUIFont(uiTitlePT, fwSemiBold, dpi)
	if body == 0 {
		return
	}
	if title == 0 {
		title = body
	}
	w.mu.Lock()
	w.font, w.fontTitle, w.dpi = body, title, dpi
	edit, filter, theme := w.edit, w.filter, w.theme
	w.mu.Unlock()
	if edit != 0 {
		procSendMessageW.Call(uintptr(edit), wmSetFont, body, 1)
		initEditColors(edit, theme) // 换字体重设一次默认字色，避免落在系统默认的黑字上
	}
	if filter != 0 {
		procSendMessageW.Call(uintptr(filter), wmSetFont, body, 1)
	}
}

// createUIFont 按磅值与字重创建 UI 字体（DPI 缩放），失败返回 0。
func createUIFont(pt, weight int32, dpi uint32) uintptr {
	px := pt * int32(dpi) / 72 // 磅 → 像素
	face := utf16Ptr(uiFaceName)
	font, _, _ := procCreateFontW.Call(
		uintptr(-px), 0, 0, 0, uintptr(weight), 0, 0, 0,
		defaultCharset, 0, 0, cleartypeQuality, 0,
		uintptr(unsafe.Pointer(face)),
	)
	return font
}

// layout 摆放子控件并刷新几何缓存。
//
// 几何全部由 computeUILayout 算出（绘制、命中判定、子控件摆放共用同一份，见
// ui_layout_windows.go），这里只负责把过滤框与日志控件挪到算好的位置。
func (w *logWindow) layout() {
	hwnd := w.handle()
	edit, filter := w.editHandle(), w.filterHandle()
	if hwnd == 0 || edit == 0 {
		return
	}
	l := w.ensureLayout()
	if l.Edit.Right <= l.Edit.Left || l.Edit.Bottom <= l.Edit.Top {
		return
	}
	// 日志控件在 WM_SIZE 之后会把视图拉回开头（RichEdit 的行为），
	// 因此先记下用户的阅读位置，缩放后再补回来。
	atBottom, first := editAtBottom(edit), editFirstVisibleLine(edit)
	moveWindow(edit, l.Edit)
	if filter != 0 {
		if l.Filter.Right > l.Filter.Left {
			moveWindow(filter, l.Filter)
			procShowWindow.Call(uintptr(filter), swShow)
		} else {
			procShowWindow.Call(uintptr(filter), swHide) // 窗口太窄：先藏起过滤框
		}
	}
	preserveEditView(edit, atBottom, first)
}

// ensureLayout 返回当前几何；客户区尺寸、DPI 或状态项一变就重算。
// 只在窗口线程调用（绘制与鼠标消息都走这里，因此不会出现两个线程同时算几何）。
func (w *logWindow) ensureLayout() uiLayout {
	hwnd := w.handle()
	if hwnd == 0 {
		return uiLayout{}
	}
	var r rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	w.mu.Lock()
	cur, dpi, okW, okH, okKey := w.uiLayout, w.dpi, w.layoutW, w.layoutH, w.layoutKey
	w.mu.Unlock()

	// 几何依赖：客户区尺寸 + DPI + 状态项的标签 + 标题与状态词（它们的宽度决定第一行怎么分）。
	// 任何一项变了就重算——否则会拿旧几何画新内容（例如标题被当成空串而不画）。
	st := w.statusSnapshot()
	key := strconv.FormatUint(uint64(dpi), 10) + "|" + st.Title + "|" + st.State + "|" +
		itemFingerprint(modeItems(st.Items))
	if okW == r.Right && okH == r.Bottom && okKey == key && cur.Regions != nil {
		return cur
	}
	l := computeUILayout(r.Right, r.Bottom, dpi, st, w.measureText, w.measureTitle)
	w.mu.Lock()
	w.uiLayout, w.layoutW, w.layoutH, w.layoutKey = l, r.Right, r.Bottom, key
	w.mu.Unlock()
	return l
}

// itemFingerprint 把状态项（动作 + 标签）拼成短串：几何只依赖它，内容变了就重算。
func itemFingerprint(items []windowStatusItem) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(it.Action)
		b.WriteByte(':')
		b.WriteString(it.Label)
		b.WriteByte('|')
	}
	return b.String()
}

// measureText 用正文字体量一段文本的像素宽度（注入给几何计算用）。
func (w *logWindow) measureText(s string) int32 {
	w.mu.Lock()
	font, title, dpi := w.font, w.fontTitle, w.dpi
	w.mu.Unlock()
	if font == 0 {
		font = title
	}
	return measureWith(w.handle(), s, font, dpi)
}

// measureTitle 用标题字体量一段文本的宽度（标题字号更大，必须单独量）。
func (w *logWindow) measureTitle(s string) int32 {
	w.mu.Lock()
	font, dpi := w.fontTitle, w.dpi
	w.mu.Unlock()
	return measureWith(w.handle(), s, font, dpi)
}

// measureWith 用给定字体量一段文本的宽度（内部借还一次窗口 DC）。
//
// 字体不可用时按字号粗估：宁可位置略偏，也不要算出 0 宽导致内容叠在一起。
func measureWith(hwnd windows.Handle, s string, font uintptr, dpi uint32) int32 {
	fallback := func() int32 { return int32(len([]rune(s))) * scaleDPI(uiFontPT+3, dpi) }
	if s == "" {
		return 0
	}
	if font == 0 {
		return fallback()
	}
	hdc, _, _ := procGetDC.Call(uintptr(hwnd))
	if hdc == 0 {
		return fallback()
	}
	old, _, _ := procSelectObject.Call(hdc, font)
	width := textWidth(hdc, s)
	procSelectObject.Call(hdc, old)
	procReleaseDC.Call(uintptr(hwnd), hdc)
	return width
}

// moveWindow 把子控件摆到算好的矩形上（bRepaint=1：位置变了就重画）。
func moveWindow(hwnd windows.Handle, r rectT) {
	if hwnd == 0 || r.Right <= r.Left || r.Bottom <= r.Top {
		return
	}
	procMoveWindow.Call(uintptr(hwnd), uintptr(r.Left), uintptr(r.Top),
		uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 1)
}

func (w *logWindow) filterHandle() windows.Handle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.filter
}

// preserveEditView 在控件尺寸变化后把阅读位置放回原处：
// 原本停在末尾的就继续停在末尾（日志仍跟随最新），往上翻看的就留在原来那一行附近。
func preserveEditView(edit windows.Handle, atBottom bool, first int32) {
	if edit == 0 {
		return
	}
	if atBottom {
		scrollEditToBottom(edit)
		return
	}
	if d := first - editFirstVisibleLine(edit); d != 0 {
		procSendMessageW.Call(uintptr(edit), emLineScroll, 0, uintptr(d))
	}
}

// paint 处理 WM_PAINT：整帧先画到内存 DC，再一次性 BitBlt 上屏。
//
// 双缓冲把「铺底色 → 画卡片 → 画状态条」合成一次可见更新，缩放窗口或状态刷新时
// 不会再看到中间态（闪烁）。内存 DC 按客户区尺寸缓存，尺寸不变就复用。
func (w *logWindow) paint(hwnd windows.Handle) {
	var ps paintStructT
	hdc, _, _ := procBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))

	var r rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	if back := w.beginFrame(hdc, r); back != 0 {
		w.paintClient(back, r)
		procBitBlt.Call(hdc, 0, 0, uintptr(r.Right), uintptr(r.Bottom), back, 0, 0, srcCopy)
		return
	}
	w.paintClient(hdc, r) // 内存 DC 建不出来时直接画（能用比完美重要）
}

// beginFrame 准备整帧离屏 DC（按客户区尺寸缓存；尺寸变了才重建）。
func (w *logWindow) beginFrame(hdc uintptr, r rectT) uintptr {
	if r.Right <= 0 || r.Bottom <= 0 {
		return 0
	}
	if w.memDC != 0 && w.memW == r.Right && w.memH == r.Bottom {
		return w.memDC
	}
	w.releaseFrame()
	mem, _, _ := procNewMemDC.Call(hdc)
	if mem == 0 {
		return 0
	}
	bmp, _, _ := procNewMemBmp.Call(hdc, uintptr(r.Right), uintptr(r.Bottom))
	if bmp == 0 {
		procFreeDC.Call(mem)
		return 0
	}
	procSelectObject.Call(mem, bmp)
	w.memDC, w.memBmp, w.memW, w.memH = mem, bmp, r.Right, r.Bottom
	return mem
}

// releaseFrame 释放离屏 DC 与位图（窗口销毁 / 尺寸变化时）。
func (w *logWindow) releaseFrame() {
	if w.memDC != 0 {
		procFreeDC.Call(w.memDC)
		w.memDC = 0
	}
	if w.memBmp != 0 {
		procFreeObject.Call(w.memBmp)
		w.memBmp = 0
	}
	w.memW, w.memH = 0, 0
}

// paintClient 把客户区里「不属于子控件」的部分画到任意 DC：顶部状态条 + 日志卡片。
// （日志控件与过滤框嵌在卡片里，卡片本身在控件外侧，这里负责它。）
//
// WM_PAINT 与 WM_PRINTCLIENT 共用这里，因此截图/打印工具（PrintWindow / PowerToys /
// 共享软件）也能拿到完整画面，而不是一片空白。
func (w *logWindow) paintClient(hdc uintptr, client rectT) {
	w.mu.Lock()
	brush := w.theme.bgBrush
	w.mu.Unlock()
	if brush != 0 {
		// 客户区始终铺一层主题底色：自绘内容必须落在不透明像素上，颜色才可控、文字才清晰
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&client)), brush)
	}
	l := w.ensureLayout()
	w.paintCard(hdc, l)
	w.paintHeader(hdc, l)
}

// paintCard 画日志卡片本体：圆角底色 + 一圈 1 像素发丝线描边 + 工具条下的分隔线。
//
// 之所以由父窗口来铺这张卡片的底：日志控件与过滤框都是方形子控件，它们的四角会盖住
// 圆角；把它们缩进卡片内部（见 layout），卡片的圆角与内边距就完全由这里决定，
// 文字不会贴着边框。外扩 1 像素是因为描边要落在子控件外侧（子控件区域被 WS_CLIPCHILDREN 挖掉）。
func (w *logWindow) paintCard(hdc uintptr, l uiLayout) {
	w.mu.Lock()
	theme, dpi := w.theme, w.dpi
	w.mu.Unlock()
	if theme.hairlinePen == 0 || theme.cardBrush == 0 {
		return
	}
	card := l.Card
	if card.Right <= card.Left || card.Bottom <= card.Top {
		return
	}
	radius := scaleDPI(uiRadiusDIP, dpi)
	// RoundRect 一次性完成「填底色 + 描边」，因此这里同时选入卡片画刷与发丝画笔
	oldBrush, _, _ := procSelectObject.Call(hdc, theme.cardBrush)
	oldPen, _, _ := procSelectObject.Call(hdc, theme.hairlinePen)
	procRoundRect.Call(hdc, uintptr(card.Left), uintptr(card.Top),
		uintptr(card.Right), uintptr(card.Bottom),
		uintptr(2*radius+2), uintptr(2*radius+2))
	procSelectObject.Call(hdc, oldPen)
	procSelectObject.Call(hdc, oldBrush)

	// 工具条与日志区之间的发丝线：让「过滤」和「内容」在视觉上分开
	if sep := l.FilterSep; sep.Right > sep.Left && theme.hairlineBrush != 0 {
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&sep)), theme.hairlineBrush)
	}

	// 过滤框的 1 像素描边：只靠底色差不够「像输入框」，加一圈发丝线把边界定出来
	// （画在子控件矩形之外，因此不会被它自己遮住）
	if f := l.Filter; f.Right > f.Left {
		drawRectOutline(hdc, rectT{Left: f.Left - 1, Top: f.Top - 1, Right: f.Right + 1, Bottom: f.Bottom + 1}, theme.hairlineBrush)
	}
}

// drawRectOutline 用给定画刷画一个 1 像素矩形描边（四条细边）。
func drawRectOutline(hdc uintptr, r rectT, brush uintptr) {
	if hdc == 0 || brush == 0 || r.Right <= r.Left || r.Bottom <= r.Top {
		return
	}
	edges := [4]rectT{
		{Left: r.Left, Top: r.Top, Right: r.Right, Bottom: r.Top + 1},
		{Left: r.Left, Top: r.Bottom - 1, Right: r.Right, Bottom: r.Bottom},
		{Left: r.Left, Top: r.Top, Right: r.Left + 1, Bottom: r.Bottom},
		{Left: r.Right - 1, Top: r.Top, Right: r.Right, Bottom: r.Bottom},
	}
	for i := range edges {
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&edges[i])), brush)
	}
}

// paintHeader 在任意 DC 上绘制顶部状态条与自绘控件（几何来自 uiLayout，与命中判定同源）。
// 与窗口无关（只用 hdc 与几何），因此单测可以画到内存 DC 上逐像素检查。
//
// 配色分工：标题=强调色，状态词=按健康度取语义色，胶囊状态点=开关语义色（开绿/关灰），
// 文字=文字色阶，交互底色只表达「悬停/按下」。
func (w *logWindow) paintHeader(hdc uintptr, l uiLayout) {
	w.mu.Lock()
	theme, dpi := w.theme, w.dpi
	font, titleFont := w.font, w.fontTitle
	status, icon := w.status, w.icon
	onlyWarn := w.onlyWarn
	w.mu.Unlock()
	toast := w.activeToast() // 已过期/被清掉的提示按「没有」处理

	if hdc == 0 || font == 0 || titleFont == 0 {
		return
	}
	procSetBkMode.Call(hdc, bkTransparent)

	// 品牌区：小图标 + 标题
	if icon != 0 {
		side := l.Icon.Right - l.Icon.Left
		procDrawIconEx.Call(hdc, uintptr(l.Icon.Left), uintptr(l.Icon.Top), uintptr(icon),
			uintptr(side), uintptr(side), 0, 0, diNormal)
	}
	if status.Title != "" && l.Title.Right > l.Title.Left {
		procSelectObject.Call(hdc, titleFont)
		procSetTextColor.Call(hdc, uintptr(theme.accent))
		drawTextInRect(hdc, status.Title, l.Title, dtLeft)
	}

	// 运行状态词：颜色与健康度同源（绿=正常、橙=降级、灰=未运行）
	if status.State != "" && l.State.Right > l.State.Left {
		procSelectObject.Call(hdc, font)
		procSetTextColor.Call(hdc, uintptr(theme.stateColor(status.Level)))
		drawTextInRect(hdc, status.State, l.State, dtLeft)
	}

	// 自绘控件：开关胶囊 + 动作按钮 + 只看警告（绘制顺序即 Regions 顺序）
	procSelectObject.Call(hdc, font)
	for _, r := range l.Regions {
		on := false
		switch {
		case r.Kind == uiChip:
			on = status.onOf(r.ID)
		case r.ID == uiIDOnlyWarn:
			on = onlyWarn
		}
		drawRegion(hdc, theme, font, r, dpi, on, w.hoverID == r.ID, w.pressID == r.ID)
	}

	// 第二行：轻量提示优先（那是刚发生的事），否则显示详情
	procSelectObject.Call(hdc, font)
	if toast.Text != "" {
		procSetTextColor.Call(hdc, uintptr(theme.styleColor(toast.Style)))
		drawTextInRect(hdc, toast.Text, l.Detail, dtLeft)
	} else if avail := l.Detail.Right - l.Detail.Left; avail > 0 {
		if text := fitDetail(func(s string) int32 { return textWidth(hdc, s) }, status.Detail, avail); text != "" {
			procSetTextColor.Call(hdc, uintptr(theme.muted))
			drawTextInRect(hdc, text, l.Detail, dtLeft)
		}
	}

	// 状态条与卡片之间的发丝分隔线：左右与日志卡片对齐，形成统一的栅格
	pad := scaleDPI(uiPadDIP, dpi)
	headH := scaleDPI(uiHeaderDIP, dpi)
	if theme.hairlineBrush != 0 {
		line := rectT{Left: pad, Top: headH - 1, Right: l.Card.Right - 1, Bottom: headH}
		if line.Right > line.Left {
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&line)), theme.hairlineBrush)
		}
	}
}

// drawTextInRect 在一个矩形里画一行文字（垂直居中、超长省略），返回实际绘制宽度。
func drawTextInRect(hdc uintptr, text string, r rectT, flags uintptr) int32 {
	if hdc == 0 || text == "" || r.Right <= r.Left || r.Bottom <= r.Top {
		return 0
	}
	p := utf16Ptr(text)
	if p == nil {
		return 0
	}
	rc := r
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), ^uintptr(0),
		uintptr(unsafe.Pointer(&rc)),
		flags|dtVCenter|dtSingleLine|dtNoPrefix|dtEndEllipsis)
	return rc.Right - rc.Left
}

// detailSep 是状态条第二行各段之间的分隔符（比全角竖线轻，一眼能分段又不抢眼）。
const detailSep = "  ·  "

// fitDetail 在可用宽度内挑选状态条第二行要显示的片段：全部放得下就全显示；
// 放不下则从尾部逐段丢弃（能多放一个「…」就补上，提示还有内容没显示）；
// 连第一段都放不下时返回空串（整条不画，宁可少显示也不和别的内容挤在一起）。
//
// 度量方式由调用方注入（真实绘制时是 GDI 的 textWidth），因此本函数可直接单测。
func fitDetail(measure func(string) int32, segs []string, avail int32) string {
	if avail <= 0 || len(segs) == 0 {
		return ""
	}
	for n := len(segs); n >= 1; n-- {
		head := strings.Join(segs[:n], detailSep)
		if n == len(segs) {
			if measure(head) <= avail {
				return head
			}
			continue
		}
		// 先试带省略号的版本（更诚实地告诉用户「这里被截断了」），再退回不带省略号的。
		for _, cand := range []string{head + "…", head} {
			if measure(cand) <= avail {
				return cand
			}
		}
	}
	return ""
}

// drawTextLine 在水平范围 [x, x+width) 内画一行垂直居中的文字（超长省略），返回实际宽度。
func drawTextLine(hdc uintptr, text string, x, cy, width int32, flags uintptr) int32 {
	p := utf16Ptr(text)
	if p == nil || width <= 0 {
		return 0
	}
	const lineH = 40 // 给足高度，让 DrawTextW 自己做垂直居中
	rect := rectT{Left: x, Top: cy - lineH/2, Right: x + width, Bottom: cy + lineH/2}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p)), ^uintptr(0),
		uintptr(unsafe.Pointer(&rect)),
		flags|dtVCenter|dtSingleLine|dtNoPrefix|dtEndEllipsis)
	return width
}

// textWidth 测量文本在当前 DC 字体下的像素宽度（调用前需先选入字体）。
func textWidth(hdc uintptr, text string) int32 {
	p := utf16Ptr(text)
	if p == nil || text == "" {
		return 0
	}
	var size struct{ CX, CY int32 }
	procTextExtent.Call(hdc, uintptr(unsafe.Pointer(p)), uintptr(len([]rune(text))),
		uintptr(unsafe.Pointer(&size)))
	return size.CX
}

// wndProc 是窗口过程。关键在 WM_CLOSE：**只隐藏、不销毁**，
// 于是「点 X = 关窗口」不会连带结束程序。
//
// lparam 用 unsafe.Pointer 而不是 uintptr：回调参数必须是指针大小，
// 而结构体参数（WM_GETMINMAXINFO / WM_DPICHANGED）要按指针解释。
func wndProc(hwnd windows.Handle, msg uint32, wparam uintptr, lparam unsafe.Pointer) uintptr {
	switch msg {
	case wmSize:
		lw.layout()
		return 0

	case wmPaint:
		lw.paint(hwnd)
		return 0

	case wmPrint, wmPrintClient:
		// 截图 / 打印工具走这条路（wparam 是目标 DC）。不处理的话，它们抓到的
		// 客户区会是空白——因为状态条是我们自绘的，DefWindowProc 并不知道要画什么。
		var r rectT
		procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
		lw.paintClient(uintptr(wparam), r)
		return 0

	case wmEraseBkgnd:
		// 背景由 WM_PAINT 负责（模糊材质要能透出来），这里不再擦一遍，顺带避免闪烁。
		return 1

	case wmExitSizeMove:
		lw.saveState()
		return 0

	case wmClose:
		lw.saveState()
		// 只关窗口，不关程序 —— 服务继续在托盘里运行，之后还能再显示回来。
		procShowWindow.Call(uintptr(hwnd), swHide)
		return 0

	case wmQueryEndSession:
		// 注销 / 关机：先把系统代理与 hosts 还原掉再放行（返回 1 = 同意结束会话）。
		runSessionEnd()
		return 1

	case wmEndSession:
		// 会话正在结束（wParam 为 0 表示本次注销/关机被取消）
		if wparam != 0 {
			runSessionEnd()
		}
		return 0

	case wmGetMinMaxInfo:
		if lparam != nil {
			mmi := (*minMaxInfoT)(lparam)
			dpi := windowDPI(hwnd)
			// 最小尺寸由界面决定：状态条两行 + 卡片工具条（过滤框 + 按钮）都要放得下
			mmi.MinTrackSize = pointT{X: scaleDPI(uiMinWDIP, dpi), Y: scaleDPI(uiMinHDIP, dpi)}
		}
		return 0

	case wmMouseMove:
		lw.onMouseMove(hwnd, uintptr(lparam))
		return 0

	case wmMouseLeave:
		lw.onMouseLeave(hwnd)
		return 0

	case wmSetCursor:
		if lw.onSetCursor(hwnd, uintptr(lparam)) {
			return 1 // TRUE：光标已由我们设定（手型）
		}
		// 其余情况交回系统处理（默认箭头）

	case wmLButtonDown:
		lw.onLButtonDown(hwnd, uintptr(lparam))
		return 0

	case wmLButtonUp:
		lw.onLButtonUp(hwnd, uintptr(lparam))
		return 0

	case wmCommand:
		// 过滤框内容变化（EN_CHANGE）：重置防抖定时器，停手后再整体重画
		if child := uint32(wparam >> 16); child == enChange && uintptr(lparam) == uintptr(lw.filterHandle()) {
			lw.onFilterChanged(hwnd)
			return 0
		}

	case wmTimer:
		lw.onTimer(hwnd, wparam)
		return 0

	case wmDestroy:
		lw.releaseFrame() // 释放双缓冲用的 GDI 对象

	case wmDpiChanged:
		// wParam 低 16 位是新 DPI；lParam 指向系统建议的新窗口矩形
		newDPI := uint32(wparam & 0xFFFF)
		if lparam != nil {
			r := (*rectT)(lparam)
			procSetWindowPos.Call(uintptr(hwnd), 0,
				uintptr(uint32(r.Left)), uintptr(uint32(r.Top)),
				uintptr(uint32(r.Right-r.Left)), uintptr(uint32(r.Bottom-r.Top)),
				swpNoZOrder|swpNoActivate)
		}
		lw.applyFont(newDPI)
		lw.layout()
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
		return 0

	case wmCtlColorStatic, wmCtlColorEdit:
		return lw.ctlColor(hwnd, msg, wparam, uintptr(lparam))

	case wmThemeChanged:
		// 系统切了浅色/深色：界面与菜单当场跟着换，无需重启
		lw.applySystemTheme(hwnd)
		return 0

	case wmContextMenu:
		lw.showContextMenu(hwnd, uintptr(lparam))
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, uintptr(lparam))
	return ret
}

// ctlColor 给卡片里的子控件上色（过滤框是普通 EDIT，走 WM_CTLCOLOREDIT；
// 日志控件是 RichEdit，自带底色，这里只是让回退路径也有正确配色）。
// 返回 0 会让子控件用空画刷绘制，因此不认识的控件交回默认处理。
func (w *logWindow) ctlColor(hwnd windows.Handle, msg uint32, hdc, child uintptr) uintptr {
	w.mu.Lock()
	theme, edit, filter := w.theme, w.edit, w.filter
	w.mu.Unlock()

	if hdc == 0 || (child != uintptr(edit) && child != uintptr(filter)) {
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), hdc, child)
		return ret
	}
	if child == uintptr(filter) {
		// 过滤框压在卡片底上：比卡片底再深/浅一档，看得出是输入框
		procSetTextColor.Call(hdc, uintptr(theme.text))
		procSetBkColor.Call(hdc, uintptr(theme.input))
		return theme.inputBrush
	}
	// 日志区底色比窗口底略亮/略暗一层，用来把日志区从窗口底上「抬」起来
	procSetTextColor.Call(hdc, uintptr(theme.text))
	procSetBkColor.Call(hdc, uintptr(theme.cardBG))
	return theme.cardBrush
}

// showContextMenu 在日志区右键时给出「复制 / 全选 / 打开配置 / 清空日志」。
func (w *logWindow) showContextMenu(hwnd windows.Handle, child uintptr) {
	w.mu.Lock()
	edit := w.edit
	w.mu.Unlock()
	if edit == 0 || child != uintptr(edit) {
		return
	}
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	const (
		idCopy   = 1
		idSelAll = 2
		idClear  = 3
		idConfig = 4
	)
	appendMenuItem(menu, mfString, idCopy, "复制")
	appendMenuItem(menu, mfString, idSelAll, "全选")
	appendMenuItem(menu, mfSeparator, 0, "")
	appendMenuItem(menu, mfString, idConfig, "打开配置文件")
	appendMenuItem(menu, mfString, idClear, "清空日志")

	var pt pointT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	switch int32(cmd) {
	case idCopy:
		procSendMessageW.Call(uintptr(edit), wmCopy, 0, 0)
	case idSelAll:
		procSendMessageW.Call(uintptr(edit), emSetSel, 0, ^uintptr(0))
	case idConfig:
		w.invokeAction(hwnd, uiIDConfig)
	case idClear:
		w.invokeAction(hwnd, uiIDClear)
	}
}

// saveState 记住窗口位置与大小（最大化状态也一起记）。
func (w *logWindow) saveState() {
	hwnd := w.handle()
	if hwnd == 0 {
		return
	}
	var wp windowPlacementT
	wp.Length = uint32(unsafe.Sizeof(wp))
	if r, _, _ := procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp))); r == 0 {
		return
	}
	r := wp.NormalPosition
	st := windowState{
		X: r.Left, Y: r.Top,
		W: r.Right - r.Left, H: r.Bottom - r.Top,
		Maximized: wp.ShowCmd == swShowMaximize,
	}
	if st.W <= 0 || st.H <= 0 {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = config.EnsureDir()
	_ = os.WriteFile(windowStatePath(), data, 0o644)
}

// savedWindowPos 返回窗口初始位置大小与是否最大化：优先用上次记住的
// （前提是这块位置仍落在某个显示器上，换过显示器就退回默认值）。
func savedWindowPos(dpi uint32) (windowPos, bool) {
	scale := func(v int32) int32 { return scaleDPI(v, dpi) }
	def := windowPos{X: cwUseDefault, Y: cwUseDefault, W: scale(860), H: scale(520)}

	data, err := os.ReadFile(windowStatePath())
	if err != nil {
		return def, false
	}
	var st windowState
	if err := json.Unmarshal(data, &st); err != nil || st.W < 200 || st.H < 150 {
		return def, false
	}
	probe := rectT{Left: st.X, Top: st.Y, Right: st.X + st.W, Bottom: st.Y + st.H}
	if r, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&probe)), monitorDefaultToNull); r == 0 {
		return def, false // 记录的位置已经不在任何显示器上
	}
	return windowPos{X: st.X, Y: st.Y, W: st.W, H: st.H}, st.Maximized
}

func windowStatePath() string { return filepath.Join(config.Dir(), "window.json") }

// —— 文本与外观小工具 ——

func setWindowText(hwnd windows.Handle, text string) {
	if p := utf16Ptr(text); p != nil {
		procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(p)))
	}
}

func setEditText(edit windows.Handle, text string) {
	if edit == 0 {
		return
	}
	setWindowText(edit, toCRLF(text))
	// WM_SETTEXT 之后插入符停在开头，要先把插入符移到末尾再滚动，
	// 否则视图会留在最上面（日志应该显示最新的尾部）。
	n, _, _ := procSendMessageW.Call(uintptr(edit), wmGetTextLen, 0, 0)
	procSendMessageW.Call(uintptr(edit), emSetSel, n, n)
	scrollEditToBottom(edit)
}

// scrollEditToBottom 把日志视图拉到末尾。
//
// 单独用 EM_SCROLLCARET 不够：RichEdit（RICHEDIT50W）在插入符已就位时并不滚动，
// 必须再补一次 WM_VSCROLL(SB_BOTTOM)。两者在普通 EDIT 上也安全。
func scrollEditToBottom(edit windows.Handle) {
	if edit == 0 {
		return
	}
	procSendMessageW.Call(uintptr(edit), emScrollCaret, 0, 0)
	procSendMessageW.Call(uintptr(edit), wmVScroll, sbBottom, 0)
}

func editFirstVisibleLine(edit windows.Handle) int32 {
	n, _, _ := procSendMessageW.Call(uintptr(edit), emGetFirstVisibleLine, 0, 0)
	return int32(n)
}

// editAtBottom 报告日志控件是否停在末尾（用户没往上翻）。
func editAtBottom(edit windows.Handle) bool {
	lines := int32(mustSend(edit, emGetLineCount))
	lh := editLineHeight(edit)
	if lh <= 0 {
		return true
	}
	var r rectT
	procGetClientRect.Call(uintptr(edit), uintptr(unsafe.Pointer(&r)))
	visible := r.Bottom / lh
	return editFirstVisibleLine(edit)+visible >= lines
}

func mustSend(hwnd windows.Handle, msg uint32) uintptr {
	r, _, _ := procSendMessageW.Call(uintptr(hwnd), uintptr(msg), 0, 0)
	return r
}

// editLineHeight 返回日志控件的行高（像素）。
func editLineHeight(edit windows.Handle) int32 {
	hdc, _, _ := procGetDC.Call(uintptr(edit))
	if hdc == 0 {
		return 0
	}
	defer procReleaseDC.Call(uintptr(edit), hdc)
	font, _, _ := procSendMessageW.Call(uintptr(edit), wmGetFont, 0, 0)
	if font == 0 {
		font, _, _ = procGetStockObject.Call(17) // DEFAULT_GUI_FONT
	}
	old, _, _ := procSelectObject.Call(hdc, font)
	// TEXTMETRICW 第一项是 tmHeight；后面还有很多字段，给足缓冲区即可。
	var tm [96]byte
	procGetTextMetricsW.Call(hdc, uintptr(unsafe.Pointer(&tm[0])))
	procSelectObject.Call(hdc, old)
	h := *(*int32)(unsafe.Pointer(&tm[0]))
	if h < 0 {
		h = -h
	}
	return h
}

// toCRLF 把 \n 统一成 \r\n：多行 EDIT 控件按 CRLF 换行。
func toCRLF(s string) string {
	if !strings.ContainsRune(s, '\n') {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// setTitleBarTheme 设置标题栏的浅色/深色（DWMWA_USE_IMMERSIVE_DARK_MODE，取不到就静默跳过）。
func setTitleBarTheme(hwnd windows.Handle, dark bool) {
	if procDwmSetWindowAttr.Find() != nil {
		return
	}
	var v int32
	if dark {
		v = 1
	}
	for _, attr := range []uintptr{dwmwaUseImmersiveDarkMode, dwmwaUseImmersiveDarkModeOld} {
		if r, _, _ := procDwmSetWindowAttr.Call(uintptr(hwnd), attr,
			uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v)); r == 0 {
			return
		}
	}
}

func setWindowTheme(hwnd windows.Handle, theme string) {
	if procUxthemeSetWindowTheme.Find() != nil {
		return
	}
	if name := utf16Ptr(theme); name != nil {
		procUxthemeSetWindowTheme.Call(uintptr(hwnd), uintptr(unsafe.Pointer(name)), 0)
	}
}

func appendMenuItem(menu uintptr, flags, id uintptr, text string) {
	label := uintptr(0)
	if text != "" {
		label = uintptr(unsafe.Pointer(utf16Ptr(text)))
	}
	procAppendMenuW.Call(menu, flags, id, label)
}

func procGetSystemMetricsInt(index int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(r))
}

// iconHandle 从 ICO 字节里挑一张最接近目标尺寸的图，生成 HICON。
func iconHandle(ico []byte, cx, cy int32, dpi uint32) windows.Handle {
	if cx <= 0 {
		cx = scaleDPI(32, dpi)
	}
	if cy <= 0 {
		cy = cx
	}
	if len(ico) < 6 {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	best, bestDiff := -1, int32(1<<30)
	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(ico) {
			break
		}
		w := int32(ico[off])
		if w == 0 {
			w = 256
		}
		diff := w - cx
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			best, bestDiff = i, diff
		}
	}
	if best < 0 {
		return 0
	}
	off := 6 + best*16
	size := binary.LittleEndian.Uint32(ico[off+8 : off+12])
	imgOff := binary.LittleEndian.Uint32(ico[off+12 : off+16])
	if int(imgOff)+int(size) > len(ico) || size == 0 {
		return 0
	}
	h, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&ico[imgOff])), uintptr(size), 1 /* fIcon */, 0x00030000,
		uintptr(cx), uintptr(cy), 0)
	return windows.Handle(h)
}

// showLogWindow 显示日志窗口（首次调用时创建）。
func showLogWindow() error { return lw.show() }

// prepareLogWindow 只创建窗口、不显示。
//
// --hidden（开机自启）用它换一个「看不见但存在」的窗口：注销 / 关机时系统会给它发
// WM_QUERYENDSESSION，通途因此能在会话结束前把系统代理与 hosts 还原干净。
func prepareLogWindow() error { return lw.ensure() }

// toggleLogWindow 显示/隐藏日志窗口（供托盘菜单与左键点击使用）。
func toggleLogWindow() error { return lw.toggle() }

// setWindowStatus 更新窗口顶部状态条（窗口未创建时先记下，创建后回填并重绘）。
func setWindowStatus(st windowStatus) { lw.setStatus(st) }

// —— 注销 / 关机时的优雅还原 ——

var (
	sessionEndMu   sync.Mutex
	sessionEndFn   func()
	sessionEndLast time.Time
)

// setSessionEndHandler 注册「注销 / 关机」时的清理回调。
//
// 窗口线程收到 WM_QUERYENDSESSION / WM_ENDSESSION 时调用它，用来在系统会话结束、
// 进程被强制结束之前，把系统代理与 hosts 还原成原来的样子。这是「无论怎么退出都
// 不留错配置」的一环：正常退出靠主流程、崩溃靠监护进程、关机靠这里。
func setSessionEndHandler(fn func()) {
	sessionEndMu.Lock()
	sessionEndFn = fn
	sessionEndMu.Unlock()
}

// runSessionEnd 触发一次会话结束清理（同一次关机只跑一遍：QUERYENDSESSION 之后
// 紧接着就是 ENDSESSION，两次都做一遍纯属浪费，还原本身要几百毫秒）。
func runSessionEnd() {
	sessionEndMu.Lock()
	fn := sessionEndFn
	if fn == nil || time.Since(sessionEndLast) < 3*time.Second {
		sessionEndMu.Unlock()
		return
	}
	sessionEndLast = time.Now()
	sessionEndMu.Unlock()
	fn()
}
