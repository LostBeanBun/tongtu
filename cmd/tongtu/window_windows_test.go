//go:build windows

package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"tongtu/internal/icon"
)

var (
	procPostMessageW  = user32.NewProc("PostMessageW")
	procIsWindow      = user32.NewProc("IsWindow")
	procGetWindowRect = user32.NewProc("GetWindowRect")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
)

// emGetCharFormat、sbTop 只在测试里用到（读回日志控件颜色、把滚动条拉到顶部），
// 因此不进产品代码。
const emGetCharFormat = 0x043A

const sbTop = 6

// bitmapInfoHeaderT 对应 BITMAPINFOHEADER。
type bitmapInfoHeaderT struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type rgbQuadT struct{ Blue, Green, Red, Reserved byte }

type bitmapInfoT struct {
	Header bitmapInfoHeaderT
	Colors [1]rgbQuadT
}

// memDC 是一块内存位图 DC，用来把自绘结果取回来逐像素检查。
type memDC struct {
	dc     uintptr
	bitmap uintptr
	old    uintptr
	bits   unsafe.Pointer
	w, h   int32
}

func newMemDC(w, h int32) *memDC {
	dc, _, _ := procCreateCompatibleDC.Call(0)
	if dc == 0 {
		return nil
	}
	bmi := bitmapInfoT{}
	bmi.Header.Size = uint32(unsafe.Sizeof(bmi.Header))
	bmi.Header.Width = w
	bmi.Header.Height = -h // 负高度 = 自上而下，方便按 (x, y) 取像素
	bmi.Header.Planes = 1
	bmi.Header.BitCount = 32
	var bits unsafe.Pointer
	bm, _, _ := procCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bmi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bm == 0 {
		procDeleteDC.Call(dc)
		return nil
	}
	old, _, _ := procSelectObject.Call(dc, bm)
	return &memDC{dc: dc, bitmap: bm, old: old, bits: bits, w: w, h: h}
}

// pixel 返回 (x, y) 处的颜色，按 0x00RRGGBB 组装（位图里是 BGRA）。
func (m *memDC) pixel(x, y int32) uint32 {
	p := (*[1 << 28]byte)(m.bits)
	i := (y*m.w + x) * 4
	return uint32(p[i+2])<<16 | uint32(p[i+1])<<8 | uint32(p[i])
}

func (m *memDC) close() {
	if m.old != 0 {
		procSelectObject.Call(m.dc, m.old)
	}
	if m.bitmap != 0 {
		procDeleteObject.Call(m.bitmap)
	}
	if m.dc != 0 {
		procDeleteDC.Call(m.dc)
	}
}

// headerLayoutFor 测试用：按给定客户区尺寸算一份几何。
//
// 绘制与命中判定共用 computeUILayout（见 ui_layout_windows.go），因此这里算出的
// 就是把状态条画到内存 DC 上时要用的同一份几何。
func headerLayoutFor(t *testing.T, w, h int32) uiLayout {
	t.Helper()
	lw.mu.Lock()
	dpi, st := lw.dpi, lw.status
	lw.mu.Unlock()
	return computeUILayout(w, h, dpi, st, lw.measureText, lw.measureTitle)
}

// TestStatusBarPaints 状态条是自绘的：配色分工必须真的落到像素上。
// 位图初始为黑，因此「某个颜色出现了多少像素」可以精确断言。深浅两套主题各画一遍
// （不依赖当前系统是深色还是浅色），颜色从主题里取，避免硬编码。
func TestStatusBarPaints(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setWindowStatus(windowStatus{
		Title: "通途 Tongtu 0.1.0",
		State: "运行中",
		Level: stateOK,
		Items: []windowStatusItem{
			{Label: "系统代理", On: true, Action: uiIDSys},
			{Label: "Hosts", On: false, Action: uiIDHosts},
			{Label: "无SNI反代", On: true, Action: uiIDMITM},
		},
		Detail: []string{"服务组 steam", "活跃隧道 3", "↓1.2 MB ↑340 KB"},
	})

	lw.mu.Lock()
	dpi := lw.dpi
	darkSet, lightSet := lw.themeDarkSet, lw.themeLightSet
	lw.mu.Unlock()
	if darkSet.okBrush == 0 || lightSet.okBrush == 0 {
		t.Fatal("两套主题的 GDI 对象都应已备好")
	}
	w, h := scaleDPI(700, dpi), scaleDPI(uiHeaderDIP, dpi)

	for _, tc := range []struct {
		name string
		th   uiTheme
	}{{"深色", darkSet}, {"浅色", lightSet}} {
		t.Run(tc.name, func(t *testing.T) {
			lw.mu.Lock()
			saved := lw.theme
			lw.theme = tc.th // 直接指定配色绘制，不受当前系统主题影响
			lw.mu.Unlock()
			defer func() { lw.mu.Lock(); lw.theme = saved; lw.mu.Unlock() }()

			m := newMemDC(w, h)
			if m == nil {
				t.Fatal("无法创建内存 DC")
			}
			defer m.close()
			lw.paintHeader(m.dc, headerLayoutFor(t, w, h))
			assertHeaderPixels(t, m, tc.th, w, h)
		})
	}
}

// assertHeaderPixels 逐像素检查状态条的配色分工是否真的画出来了。
func assertHeaderPixels(t *testing.T, m *memDC, theme uiTheme, w, h int32) {
	t.Helper()

	count := map[uint32]int{}
	rows := map[uint32]map[int32]int{} // 颜色 → 行号 → 像素数（用于定位分隔线）
	for y := int32(0); y < h; y++ {
		for x := int32(0); x < w; x++ {
			c := m.pixel(x, y)
			count[c]++
			if rows[c] == nil {
				rows[c] = map[int32]int{}
			}
			rows[c][y]++
		}
	}
	// 主题色是 COLORREF(0x00BBGGRR)，位图取出来的是 0x00RRGGBB，比较前先换字节序
	okRGB, offRGB := colorRefToRGB(theme.ok), colorRefToRGB(theme.off)
	hairRGB := colorRefToRGB(theme.hairline)

	// 状态点是实心圆（GDI 不做抗锯齿），因此颜色应当精确出现且数量够多
	// （胶囊里半径 4 DIP → 直径 8 DIP，面积约 50 像素；文字抗锯齿最多蹭到几个）
	for _, want := range []struct {
		name string
		c    uint32
		min  int
	}{
		{"开启项的状态点（绿）", okRGB, 20},
		{"关闭项的状态点（灰）", offRGB, 20},
	} {
		if got := count[want.c]; got < want.min {
			t.Errorf("%s 只用 %d 个像素画出（颜色 %#06x），至少要 %d 个",
				want.name, got, want.c, want.min)
		}
	}

	// 分隔线：一条铺满「卡片宽度」的 1 像素发丝线，位于状态条最后一行
	if got := count[hairRGB]; got < int(w)/2 {
		t.Errorf("状态条下方的分隔线没有画出来：发丝色只有 %d 像素", got)
	}
	sepRow := h - 1
	if got := rows[hairRGB][sepRow]; got < int(w)/2 {
		t.Errorf("分隔线应在状态条最后一行（y=%d），实际该行只有 %d 像素", sepRow, got)
	}

	// 文字：标题用强调色（偏蓝），开关名用中性文字色（不偏色）
	var accentish, textish int
	for c, n := range count {
		r, g, b := byte(c>>16), byte(c>>8), byte(c)
		if b > g && g > r && int(b)-int(r) > 60 {
			accentish += n
		}
		if absInt(int(r)-int(g)) < 12 && absInt(int(g)-int(b)) < 12 && r > 60 {
			textish += n
		}
	}
	if accentish == 0 {
		t.Error("标题没有画出强调色（偏蓝）")
	}
	if textish == 0 {
		t.Error("开关名/正文没有画出中性文字色")
	}
	t.Logf("状态条：%d 种颜色；绿点 %d、灰点 %d、发丝线 %d（y=%d）、强调色系 %d、文字色系 %d",
		len(count), count[okRGB], count[offRGB], count[hairRGB], sepRow, accentish, textish)
}

// colorRefToRGB 把 COLORREF(0x00BBGGRR) 换成 0x00RRGGBB，便于与位图像素直接比较。
func colorRefToRGB(c uint32) uint32 {
	return (c&0xFF0000)>>16 | (c & 0x00FF00) | (c&0x0000FF)<<16
}

// TestStatusWordColorFollowsHealth 状态词的颜色必须跟着健康度走：
// 正常绿、降级橙、未运行用次要灰——这是「颜色表意」的关键一环。
func TestStatusWordColorFollowsHealth(t *testing.T) {
	for _, th := range []struct {
		name string
		th   uiTheme
	}{{"深色", themeDark}, {"浅色", themeLight}} {
		if got, want := th.th.stateColor(stateOK), th.th.ok; got != want {
			t.Errorf("%s：正常状态词应为绿色，实际 %#06x", th.name, got)
		}
		if got, want := th.th.stateColor(stateDegraded), th.th.warn; got != want {
			t.Errorf("%s：降级状态词应为橙色，实际 %#06x", th.name, got)
		}
		if got, want := th.th.stateColor(stateOff), th.th.muted; got != want {
			t.Errorf("%s：未运行状态词应为次要灰，实际 %#06x", th.name, got)
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestLogCardFilledAndInset 日志卡片的样子由父窗口决定：圆角实心底色 + 一圈发丝线，
// 日志控件嵌在卡片里、四周留出内边距（文字与滚动条不贴边框）。
//
// 之所以不再用 SetWindowRgn 给控件切圆角：控件是方的，四角会盖住圆角；现在卡片由父窗口
// 铺底、控件内缩，圆角与内边距一起有了，而且不再需要给控件设区域。
func TestLogCardFilledAndInset(t *testing.T) {
	if err := prepareLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	lw.mu.Lock()
	hwnd, edit, dpi, theme := lw.hwnd, lw.edit, lw.dpi, lw.theme
	lw.mu.Unlock()
	if edit == 0 {
		t.Fatal("日志控件不存在")
	}

	var cr rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cr)))
	if cr.Right < 100 || cr.Bottom < 100 {
		t.Fatalf("客户区尺寸异常：%dx%d", cr.Right, cr.Bottom)
	}
	pad := scaleDPI(uiPadDIP, dpi)
	headH := scaleDPI(uiHeaderDIP, dpi)
	inset := scaleDPI(uiCardPadDIP, dpi)
	cardW := cr.Right - 2*pad
	cardH := cr.Bottom - (pad + headH) - pad
	l := headerLayoutFor(t, cr.Right, cr.Bottom)

	// 1) 日志控件尺寸 = 几何算出的矩形（卡片内边距里、工具条下方）。
	//
	// 这里比较「窗口矩形」而不是「客户区矩形」：竖向滚动条出现时会占掉客户区宽度，
	// 但控件窗口本身仍是几何给出的那块地方（用客户区比较会让测试依赖当时日志有多长）。
	var ew rectT
	procGetWindowRect.Call(uintptr(edit), uintptr(unsafe.Pointer(&ew)))
	erW, erH := ew.Right-ew.Left, ew.Bottom-ew.Top
	wantW, wantH := l.Edit.Right-l.Edit.Left, l.Edit.Bottom-l.Edit.Top
	if absInt(int(erW-wantW)) > 2 || absInt(int(erH-wantH)) > 2 {
		t.Errorf("日志控件尺寸与几何不符：控件 %dx%d，期望约 %dx%d", erW, erH, wantW, wantH)
	}
	if inset <= 0 {
		t.Fatal("内边距必须为正，否则文字会贴着卡片边框")
	}
	// 工具条占掉卡片顶部一条：日志控件因此比「卡片内边距」矮一截，但仍占卡片大部分高度
	toolbar := scaleDPI(uiToolbarDIP, dpi)
	if erH >= cardH-2*inset {
		t.Errorf("日志控件没有给卡片内的工具条让出位置：控件高 %d，卡片内高 %d", erH, cardH-2*inset)
	}
	if erH <= (cardH-2*inset-toolbar)*9/10 {
		t.Errorf("日志控件被工具条挤得太小：控件高 %d，扣掉工具条后应约 %d",
			erH, cardH-2*inset-toolbar)
	}

	// 2) 画一遍卡片：内部实心铺底色、四角被圆角切掉、上沿与工具条下方有发丝线
	m := newMemDC(cr.Right, cr.Bottom)
	if m == nil {
		t.Fatal("无法创建内存 DC")
	}
	defer m.close()
	// 按真实绘制顺序：先铺窗口底色，再画卡片
	procFillRect.Call(m.dc, uintptr(unsafe.Pointer(&cr)), theme.bgBrush)
	lw.paintCard(m.dc, l)

	cardRGB, bgRGB, hairRGB := colorRefToRGB(theme.cardBG), colorRefToRGB(theme.bg), colorRefToRGB(theme.hairline)
	top := pad + headH
	if got := m.pixel(pad+inset+2, top+inset+2); got != cardRGB {
		t.Errorf("卡片内部应铺卡片底色：像素为 %#06x", got)
	}
	if got := m.pixel(pad+1, top+1); got != bgRGB {
		t.Errorf("卡片四角应被圆角切掉（露出窗口底色）：像素为 %#06x", got)
	}
	// 内边距那一圈也必须是卡片底色（控件内缩后露出来的就是它）
	if got := m.pixel(pad+inset/2, top+inset/2); got != cardRGB {
		t.Errorf("内边距区域应是卡片底色：像素为 %#06x", got)
	}
	if got := m.pixel(cardW/2, top-1); got != hairRGB {
		t.Errorf("卡片上沿应有 1 像素发丝线：像素为 %#06x", got)
	}
	// 工具条与日志区之间也有一条发丝线（过滤框与内容在视觉上分开）
	sep := l.FilterSep
	if got := m.pixel(sep.Left+10, sep.Top); got != hairRGB {
		t.Errorf("工具条下方应有 1 像素发丝线：像素为 %#06x", got)
	}
}

// TestLogWindowCloseHidesOnly 是本窗口的核心回归测试：
// 点窗口的 X（WM_CLOSE）必须只把窗口**隐藏**，窗口与进程都还在——服务和托盘因此能继续运行。
// 这正是控制台窗口做不到、所以要自绘窗口的原因。
func TestLogWindowCloseHidesOnly(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口（例如非交互式窗口站）: %v", err)
	}
	hwnd := lw.handle()
	if hwnd == 0 {
		t.Fatal("窗口句柄无效")
	}
	if !lw.visible() {
		t.Fatal("showLogWindow 之后窗口应可见")
	}

	// 日志区应占满「状态条下方」的客户区（只允许四周留白、卡片工具条与滚动条）。
	// 初始布局不能只留给 WM_SIZE：子控件创建晚于窗口的 WM_SIZE。
	var wr, er rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wr)))
	procGetClientRect.Call(uintptr(lw.editHandle()), uintptr(unsafe.Pointer(&er)))
	lw.mu.Lock()
	pad, dpi := scaleDPI(uiPadDIP, lw.dpi), lw.dpi
	lw.mu.Unlock()
	l := headerLayoutFor(t, wr.Right, wr.Bottom)
	wantH := l.Edit.Bottom - l.Edit.Top
	if er.Bottom < wantH*9/10 {
		t.Errorf("日志控件高度不足：期望约 %d，实际 %d（DPI %d）", wantH, er.Bottom, dpi)
	}
	if er.Right < (wr.Right-2*pad)*9/10 {
		t.Errorf("日志控件宽度不足：窗口 %d，控件 %d（留白 %d）", wr.Right, er.Right, pad)
	}
	// 卡片工具条（过滤框）也要占到位：它得在卡片内、日志区上方
	if l.Filter.Right > l.Filter.Left {
		if l.Filter.Top <= l.Card.Top || l.Filter.Bottom >= l.Edit.Top {
			t.Errorf("过滤框位置不对：filter=%+v card=%+v edit=%+v", l.Filter, l.Card, l.Edit)
		}
	} else {
		t.Errorf("默认窗口宽度下应能放下过滤框（卡片宽 %d，DPI %d）", l.Card.Right-l.Card.Left, dpi)
	}

	// 日志应能写进窗口
	_, _ = lw.Write([]byte("selftest 一行日志\n"))
	if n, _, _ := procSendMessageW.Call(uintptr(lw.editHandle()), wmGetTextLen, 0, 0); n == 0 {
		t.Fatal("日志控件里应该有文本")
	}

	// 模拟点 × / Alt+F4
	procPostMessageW.Call(uintptr(hwnd), wmClose, 0, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && lw.visible() {
		time.Sleep(20 * time.Millisecond)
	}
	if lw.visible() {
		t.Fatal("收到 WM_CLOSE 后窗口应被隐藏")
	}
	if r, _, _ := procIsWindow.Call(uintptr(hwnd)); r == 0 {
		t.Fatal("窗口被销毁了：进程会随之失去界面，必须只隐藏")
	}

	// 再从托盘（或左键点图标）调出来
	if err := toggleLogWindow(); err != nil {
		t.Fatalf("再次显示窗口失败: %v", err)
	}
	if !lw.visible() {
		t.Fatal("toggle 之后窗口应重新可见")
	}
}

// TestWindowHeader 顶部状态条要能显示并更新内容。
// 状态条是自绘的（不是 STATIC 控件），因此这里校验窗口持有的状态内容。
func TestWindowHeader(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	st := windowStatus{
		Title: "通途 Tongtu 0.1.0",
		State: "运行中",
		Items: []windowStatusItem{
			{Label: "系统代理", On: true},
			{Label: "Hosts", On: false},
		},
		Detail: []string{"服务组 steam", "活跃隧道 2"},
	}
	setWindowStatus(st)
	if got := lw.statusSnapshot(); got.key() != st.key() {
		t.Errorf("状态条内容不符：%+v != %+v", got, st)
	}
	// 重复设置同样的内容不做无谓刷新，但内容必须保持正确
	setWindowStatus(st)
	if got := lw.statusSnapshot(); got.key() != st.key() {
		t.Errorf("状态条内容被改动：%+v", got)
	}
	// 状态变化后应当更新
	st.Items[0].On = false
	setWindowStatus(st)
	if got := lw.statusSnapshot(); got.key() != st.key() {
		t.Errorf("状态条未随状态更新：%+v != %+v", got, st)
	}
}

// TestFitDetail 状态条第二行的信息在窄窗口下要「优雅降级」：
// 放不下时按段从尾部丢弃（能补省略号就补），而不是第一次放不下就整条消失。
func TestFitDetail(t *testing.T) {
	segs := []string{"服务组 steam、github", "活跃隧道 3", "↓1.2 MB ↑340 KB"}
	full := strings.Join(segs, detailSep)
	head := strings.Join(segs[:2], detailSep)
	one := segs[0]
	measure := func(s string) int32 { return int32(len([]rune(s))) * 10 } // 每字 10 像素的假度量

	if got := fitDetail(measure, segs, measure(full)); got != full {
		t.Errorf("宽度足够时应全部显示，得到 %q", got)
	}
	if got := fitDetail(measure, segs, measure(head)); got != head {
		t.Errorf("只放得下前两段时应丢掉尾段，得到 %q", got)
	}
	if got := fitDetail(measure, segs, measure(head+"…")); got != head+"…" {
		t.Errorf("还有余量放省略号时应提示被截断，得到 %q", got)
	}
	if got := fitDetail(measure, segs, measure(one)); got != one {
		t.Errorf("只放得下第一段时应只画第一段，得到 %q", got)
	}
	if got := fitDetail(measure, segs, measure(one)); got == full {
		t.Error("窄窗口下不应硬画全部（会与别的内容重叠）")
	}
	if got := fitDetail(measure, segs, 5); got != "" {
		t.Errorf("连第一段都放不下时不应画任何东西，得到 %q", got)
	}
	if got := fitDetail(measure, nil, 500); got != "" {
		t.Errorf("没有信息片段时不应画任何东西，得到 %q", got)
	}
}

// TestNarrowHeaderKeepsDetail 窄窗口下第二行的信息必须「截断显示」而不是整条消失。
func TestNarrowHeaderKeepsDetail(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setWindowStatus(windowStatus{
		Title: "通途 Tongtu 0.1.0",
		State: "运行中",
		Level: stateOK,
		Items: []windowStatusItem{
			{Label: "系统代理", On: true},
			{Label: "Hosts", On: false},
			{Label: "无SNI反代", On: true},
		},
		Detail: []string{"服务组 steam、github", "活跃隧道 3", "↓1.2 MB ↑340 KB"},
	})

	lw.mu.Lock()
	dpi, theme := lw.dpi, lw.theme
	lw.mu.Unlock()

	w, h := scaleDPI(700, dpi), scaleDPI(uiHeaderDIP, dpi)
	m := newMemDC(w, h)
	if m == nil {
		t.Fatal("无法创建内存 DC")
	}
	defer m.close()
	lw.paintHeader(m.dc, headerLayoutFor(t, w, h))

	// 右侧信息用 theme.muted 画，数一下位图上的精确像素。
	want := colorRefToRGB(theme.muted)
	n := 0
	for y := int32(0); y < h; y++ {
		for x := int32(0); x < w; x++ {
			if m.pixel(x, y) == want {
				n++
			}
		}
	}
	if n == 0 {
		t.Error("窄窗口下右侧信息整条消失了：位图上没有 muted 色的文字像素")
	}
	t.Logf("窄窗口右侧信息：muted 色像素 %d", n)
}

// TestAppendKeepsViewAndSelection 覆盖日志区的两个「不打断用户」行为：
//   - 用户往上翻看历史时，新日志不应把视图拽回末尾；
//   - 用户正在划选文本时，新日志不应丢掉选区。
func TestAppendKeepsViewAndSelection(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	edit := lw.editHandle()
	if edit == 0 {
		t.Fatal("日志控件不可用")
	}

	// 铺满日志并回到末尾
	setEditText(edit, strings.Repeat("已有日志一行\r\n", 400))
	if !editAtBottom(edit) {
		t.Fatal("setEditText 之后应停在末尾")
	}

	// 往上翻看后追加：视图不应被拽回末尾
	up := int32(-30)
	procSendMessageW.Call(uintptr(edit), emLineScroll, 0, uintptr(up))
	if editAtBottom(edit) {
		t.Fatal("往上滚动之后不应判定为停在末尾")
	}
	first := editFirstVisibleLine(edit)
	_, _ = lw.Write([]byte("翻看期间产生的新日志\n"))
	if got := editFirstVisibleLine(edit); got != first {
		t.Errorf("往上翻看时追加日志不应改变视野：首行 %d → %d", first, got)
	}

	// 划选后追加：选区应原样保留（否则复制会被打断）
	procSendMessageW.Call(uintptr(edit), emSetSel, 0, 4)
	_, _ = lw.Write([]byte("划选期间产生的新日志\n"))
	start, end := editSelection(edit)
	if start != 0 || end != 4 {
		t.Errorf("追加日志不应丢掉选区：期望 0..4，实际 %d..%d", start, end)
	}
}

// TestWindowStatePersist 窗口位置与大小要能记住（重启后仍用上次的位置）。
func TestWindowStatePersist(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	path := windowStatePath()
	old, readErr := os.ReadFile(path)
	defer func() {
		if readErr == nil {
			_ = os.WriteFile(path, old, 0o644)
			return
		}
		_ = os.Remove(path)
	}()

	lw.saveState()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("窗口状态未落盘: %v", err)
	}
	var st windowState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("窗口状态不是合法 JSON: %v", err)
	}
	if st.W < 200 || st.H < 150 {
		t.Fatalf("保存的窗口尺寸不合理: %+v", st)
	}
	// 记录的尺寸应能被读回（且仍落在某块显示器上）
	got, _ := savedWindowPos(96)
	if got.W != st.W || got.H != st.H {
		t.Errorf("读回的尺寸与保存的不一致: %+v vs %+v", got, st)
	}
	if got.X != st.X || got.Y != st.Y {
		t.Errorf("读回的位置与保存的不一致: %+v vs %+v", got, st)
	}
}

// TestDefaultWindowPos 默认窗口大小必须是设定值，且默认位置用 CW_USEDEFAULT。
// 回归点：曾经把默认位置与宽高放在同一个 rect 里，用 Right-Left 算宽度，
// CW_USEDEFAULT（很大的负数）相减溢出 int32，窗口退化成最小尺寸 460x260。
func TestDefaultWindowPos(t *testing.T) {
	path := windowStatePath()
	old, readErr := os.ReadFile(path)
	_ = os.Remove(path)
	defer func() {
		if readErr == nil {
			_ = os.WriteFile(path, old, 0o644)
		}
	}()

	got, max := savedWindowPos(96)
	if max {
		t.Error("没有历史状态时不应处于最大化")
	}
	if got.X != cwUseDefault || got.Y != cwUseDefault {
		t.Errorf("默认位置应为 CW_USEDEFAULT：%+v", got)
	}
	if got.W != 860 || got.H != 520 {
		t.Errorf("默认窗口大小应为 860x520：%+v", got)
	}
	if scaled, _ := savedWindowPos(144); scaled.W != 1290 || scaled.H != 780 {
		t.Errorf("默认大小应按 DPI 缩放（144 DPI → 1290x780）：%+v", scaled)
	}
}

// TestThemeSwitchReusesBrushes 回归保护：系统在浅色/深色之间切换时（WM_THEMECHANGED）
// 界面要当场换装，而且**不能**每次切换都新建 GDI 画刷（否则反复切换会泄漏 GDI 对象）。
func TestThemeSwitchReusesBrushes(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	lw.mu.Lock()
	hwnd, dark, light := lw.hwnd, lw.themeDarkSet, lw.themeLightSet
	lw.mu.Unlock()
	if dark.bgBrush == 0 || light.bgBrush == 0 || dark.cardBrush == 0 || light.cardBrush == 0 {
		t.Fatal("深浅两套主题的画刷都应当在创建窗口时准备好")
	}

	// 先故意置成另一套，再让窗口自己处理「系统主题已改变」
	lw.mu.Lock()
	lw.theme = light
	lw.mu.Unlock()
	procSendMessageW.Call(uintptr(hwnd), wmThemeChanged, 0, 0)

	want := light
	if systemThemeDark() {
		want = dark
	}
	lw.mu.Lock()
	got := lw.theme
	lw.mu.Unlock()
	if got.bg != want.bg || got.cardBG != want.cardBG {
		t.Errorf("切换后用的不是系统对应配色：bg=%#x card=%#x，期望 %#x / %#x",
			got.bg, got.cardBG, want.bg, want.cardBG)
	}
	if got.bgBrush != want.bgBrush || got.cardBrush != want.cardBrush ||
		got.okBrush != want.okBrush || got.offBrush != want.offBrush {
		t.Error("切换主题应整套复用预先建好的画刷，不应重新创建 GDI 对象")
	}
}

// TestNoWindowHelpers 覆盖不依赖真实窗口的纯逻辑。
func TestNoWindowHelpers(t *testing.T) {
	if got, want := rgb(31, 31, 31), uint32(0x001F1F1F); got != want {
		t.Errorf("rgb(31,31,31) = %#x, 期望 %#x", got, want)
	}
	// COLORREF 是 0x00BBGGRR：蓝色落在最高字节（R 在最低字节）
	if got, want := rgb(108, 182, 255), uint32(0x00FFB66C); got != want {
		t.Errorf("rgb(108,182,255) = %#x, 期望 %#x", got, want)
	}
	if got := scaleDPI(28, 96); got != 28 {
		t.Errorf("scaleDPI(28,96) = %d, 期望 28", got)
	}
	if got := scaleDPI(28, 144); got != 42 {
		t.Errorf("scaleDPI(28,144) = %d, 期望 42", got)
	}
	if got := toCRLF("a\nb"); got != "a\r\nb" {
		t.Errorf("toCRLF 未转换换行: %q", got)
	}
	if got := toCRLF("a\r\nb"); got != "a\r\nb" {
		t.Errorf("toCRLF 破坏了已有的 CRLF: %q", got)
	}
	if got := toCRLF("无换行"); got != "无换行" {
		t.Errorf("toCRLF 改动了不含换行的文本: %q", got)
	}
	if iconHandle(icon.ICO, 32, 32, 96) == 0 {
		t.Error("从 ICO 生成窗口图标失败")
	}
	setDPIAware() // 不应 panic

	// 菜单主题：uxtheme 的未公开导出取不到时应静默跳过，不应 panic
	enableDarkMenus()

	// 深色 / 浅色必须是两套真正不同的配色（界面跟随系统主题的基础）
	if !themeDark.dark || themeLight.dark {
		t.Error("主题的 dark 标记不正确")
	}
	if themeDark.bg == themeLight.bg || themeDark.cardBG == themeLight.cardBG || themeDark.text == themeLight.text {
		t.Error("深色与浅色主题必须是两套不同的配色")
	}
	// 「开」的状态点应偏绿，强调色（iOS systemBlue）应偏蓝
	if r, g, b := byte(themeDark.ok), byte(themeDark.ok>>8), byte(themeDark.ok>>16); g <= r || g <= b {
		t.Errorf("「开」的状态点应当偏绿：%#x", themeDark.ok)
	}
	if r, g, b := byte(themeDark.accent), byte(themeDark.accent>>8), byte(themeDark.accent>>16); b <= r || b <= g {
		t.Errorf("强调色应当偏蓝：%#x", themeDark.accent)
	}
	// 日志卡片与窗口底必须是不同层次的颜色（卡片要「浮」起来）
	if themeDark.cardBG == themeDark.bg || themeLight.cardBG == themeLight.bg {
		t.Error("日志卡片底色应与窗口底色分层")
	}

	// 跟随系统主题：systemThemeDark 必须与注册表里的 AppsUseLightTheme 一致
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err == nil {
		if v, _, verr := k.GetIntegerValue("AppsUseLightTheme"); verr == nil {
			if got, want := systemThemeDark(), v == 0; got != want {
				t.Errorf("systemThemeDark() = %v，但 AppsUseLightTheme = %d", got, v)
			}
		}
		_ = k.Close()
	}
}

// TestSessionEndRunsOnce 注销 / 关机时窗口会先收到 WM_QUERYENDSESSION、紧接着再来一个
// WM_ENDSESSION；还原动作只能做一次（还原本身要几百毫秒），但必须真的被触发。
func TestSessionEndRunsOnce(t *testing.T) {
	defer setSessionEndHandler(nil)
	defer func() {
		sessionEndMu.Lock()
		sessionEndLast = time.Time{}
		sessionEndMu.Unlock()
	}()

	calls := make(chan struct{}, 8)
	setSessionEndHandler(func() { calls <- struct{}{} })

	runSessionEnd() // 对应 WM_QUERYENDSESSION
	runSessionEnd() // 对应紧随其后的 WM_ENDSESSION
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("注销/关机时应触发一次优雅还原")
	}
	select {
	case <-calls:
		t.Fatal("同一次关机只应还原一次")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestLogCardEdgeDrawn 日志卡片外围要有一圈 1 像素发丝描边（卡片在模糊材质上才有清晰边界）。
// 关键点：描边必须画在子控件**外侧**（子控件区域被 WS_CLIPCHILDREN 挖掉），且四个角是圆的。
func TestLogCardEdgeDrawn(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	lw.mu.Lock()
	hwnd, edit, dpi, theme := lw.hwnd, lw.edit, lw.dpi, lw.theme
	lw.mu.Unlock()
	if edit == 0 {
		t.Fatal("日志控件不存在")
	}

	var cr rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cr)))
	if cr.Right < 100 || cr.Bottom < 100 {
		t.Fatalf("客户区尺寸异常：%dx%d", cr.Right, cr.Bottom)
	}
	m := newMemDC(cr.Right, cr.Bottom)
	if m == nil {
		t.Fatal("无法创建内存 DC")
	}
	defer m.close()
	lw.paintCard(m.dc, headerLayoutFor(t, cr.Right, cr.Bottom))

	pad, headH := scaleDPI(uiPadDIP, dpi), scaleDPI(uiHeaderDIP, dpi)
	hair := colorRefToRGB(theme.hairline)
	cardRGB := colorRefToRGB(theme.cardBG)
	var total, filled int
	for y := int32(0); y < cr.Bottom; y++ {
		for x := int32(0); x < cr.Right; x++ {
			switch m.pixel(x, y) {
			case hair:
				total++
			case cardRGB:
				filled++
			}
		}
	}
	// 两条长边（上下）就有约 2×卡片宽度的像素，再算上两条短边
	cardW := cr.Right - 2*pad
	if total < int(2*cardW) {
		t.Errorf("卡片描边像素太少（%d），看不出是一圈线", total)
	}
	// 卡片本体是实心铺底的，否则圆角处会露出窗口底、内边距也就没有底色
	if filled < int(cardW)*2 {
		t.Errorf("卡片没有铺底色（卡片底色只有 %d 像素）", filled)
	}
	// 上边：应恰好落在卡片上沿的上一行
	top := pad + headH - 1
	var inRow int
	for x := int32(0); x < cr.Right; x++ {
		if m.pixel(x, top) == hair {
			inRow++
		}
	}
	if inRow < int(cardW)/2 {
		t.Errorf("卡片上沿（y=%d）的描边只有 %d 像素，期望约 %d", top, inRow, cardW)
	}
	// 圆角：描边矩形的左上角不该有像素（否则就是直角边框）
	if m.pixel(pad-1, top) == hair {
		t.Error("卡片描边是直角：左上角应当被圆角切掉")
	}
	// 描边必须在子控件外面：卡片内部第一个像素不能是描边（否则会被子控件盖住/划进卡片里）
	if m.pixel(pad, pad+headH) == hair {
		t.Error("描边画到了卡片内部：应整体外扩 1 像素（子控件区域会被挖掉）")
	}
	t.Logf("卡片描边：共 %d 像素，上沿 y=%d 有 %d 像素（卡片宽 %d）", total, top, inRow, cardW)
}

// TestWindowPrintsToDC 截图/打印工具（PrintWindow、共享软件）走 WM_PRINTCLIENT：
// 必须真的把自绘的状态条与卡片描边画到给定 DC 上，否则它们抓到的客户区是空白。
func TestWindowPrintsToDC(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	setWindowStatus(windowStatus{
		Title: "通途 Tongtu 0.1.0",
		State: "部分生效",
		Level: stateDegraded,
		Items: []windowStatusItem{
			{Label: "系统代理", On: true},
			{Label: "Hosts", On: false},
		},
		Detail: []string{"服务组 steam"},
	})

	lw.mu.Lock()
	hwnd, dpi, theme := lw.hwnd, lw.dpi, lw.theme
	lw.mu.Unlock()

	var cr rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cr)))
	if cr.Right < 100 || cr.Bottom < 100 {
		t.Fatalf("客户区尺寸异常：%dx%d", cr.Right, cr.Bottom)
	}
	m := newMemDC(cr.Right, cr.Bottom)
	if m == nil {
		t.Fatal("无法创建内存 DC")
	}
	defer m.close()

	// 和 PrintWindow 内部做的事一样：把内存 DC 交给窗口
	procSendMessageW.Call(uintptr(hwnd), wmPrintClient, uintptr(m.dc), 0)

	okRGB, hairRGB := colorRefToRGB(theme.ok), colorRefToRGB(theme.hairline)
	var dots, hair int
	pad := scaleDPI(uiPadDIP, dpi)
	for y := int32(0); y < cr.Bottom; y++ {
		for x := int32(0); x < cr.Right; x++ {
			switch m.pixel(x, y) {
			case okRGB:
				dots++
			case hairRGB:
				hair++
			}
		}
	}
	// 一个「开」的状态点 + 卡片四周的描边
	if dots < 20 {
		t.Errorf("WM_PRINTCLIENT 没有画出状态点（绿色只有 %d 像素）", dots)
	}
	if hair < int(cr.Right-2*pad) {
		t.Errorf("WM_PRINTCLIENT 没有画出卡片描边（发丝色只有 %d 像素）", hair)
	}
	t.Logf("截屏路径：状态点 %d 像素、描边 %d 像素（客户区 %dx%d）", dots, hair, cr.Right, cr.Bottom)
}

// TestPreserveEditView 缩放后「保住阅读位置」的补位逻辑：
// 原本停在末尾的补回末尾（继续跟随最新），往上翻看的补回原来那一行。
//
// 背景：RichEdit 在 WM_SIZE 之后会把视图重置，真机上表现为「改变窗口大小后日志跳回开头」。
func TestPreserveEditView(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	edit := lw.editHandle()
	if edit == 0 {
		t.Fatal("日志控件不可用")
	}
	setEditText(edit, strings.Repeat("日志一行\r\n", 60))

	// 停在末尾：补位后仍应停在末尾
	scrollEditToBottom(edit)
	if !editAtBottom(edit) {
		t.Fatal("前置条件：铺满日志后应停在末尾")
	}
	preserveEditView(edit, true, 0)
	if !editAtBottom(edit) {
		t.Error("原本停在末尾时，补位后应继续停在末尾")
	}

	// 往上翻看：模拟 WM_SIZE 把视图拉回开头，补位应放回原来那一行
	up := int32(-25)
	procSendMessageW.Call(uintptr(edit), emLineScroll, 0, uintptr(up))
	first := editFirstVisibleLine(edit)
	if first == 0 || editAtBottom(edit) {
		t.Fatalf("前置条件：往上滚动之后应停在中间（首行 %d）", first)
	}
	procSendMessageW.Call(uintptr(edit), wmVScroll, sbTop, 0)
	if got := editFirstVisibleLine(edit); got != 0 {
		t.Fatalf("前置条件：应已回到开头，实际首行 %d", got)
	}
	preserveEditView(edit, false, first)
	if got := editFirstVisibleLine(edit); got != first {
		t.Errorf("往上翻看时补位应回到原来那一行：%d → %d", first, got)
	}
	if editAtBottom(edit) {
		t.Error("往上翻看时补位不应把视图拽到末尾")
	}
}

// TestLogLinesUseThemeColors 日志内容真的按级别上色了：富文本控件（RICHEDIT50W）的
// 字符格式可以直接读回，因此「警告=橙、错误=红、正文=一级文字色」不必靠人眼看图。
//
// 顺便验证换主题时日志会整体重画换色——系统深浅色不能随便切换，这里直接换配色画一遍。
func TestLogLinesUseThemeColors(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	if !richEditActive() {
		t.Skip("当前环境没有富文本控件（msftedit 不可用），日志只有纯文本")
	}
	edit := lw.editHandle()
	if edit == 0 {
		t.Fatal("日志控件不可用")
	}
	if richEditModule() == 0 {
		t.Error("富文本可用却没有 msftedit.dll 句柄：加载状态不一致")
	}

	// 只放一条错误日志：字符位置可预测，便于取「级别词」与「正文」这两段来读颜色
	lines := formatRecord(rec(slog.LevelError, "启动失败"), nil, "")
	lw.mu.Lock()
	lw.lines, lw.lineBytes = lines, 0
	for _, l := range lines {
		lw.lineBytes += l.size()
	}
	theme, lightSet := lw.theme, lw.themeLightSet
	lw.mu.Unlock()
	lw.renderAll()

	// 布局：时间(8) + 两空格 + 级别词(2, 10..12) + 两空格 + 正文(14..)
	if got := editCharColorAt(edit, 10, 12); got != theme.danger {
		t.Errorf("错误级别词应为错误红 %#06x，实际 %#06x", theme.danger, got)
	}
	if got := editCharColorAt(edit, 14, 16); got != theme.text {
		t.Errorf("日志正文应为一级文字色 %#06x，实际 %#06x", theme.text, got)
	}
	if got := editCharColorAt(edit, 0, 2); got != theme.muted {
		t.Errorf("时间戳应为次要色 %#06x，实际 %#06x", theme.muted, got)
	}

	// 控件底色必须跟着卡片底色（否则深色主题下会是一条白底）
	if got, _, _ := procSendMessageW.Call(uintptr(edit), emSetBkColor, 0, uintptr(theme.cardBG)); uint32(got) != theme.cardBG {
		t.Errorf("日志控件底色应为卡片底色 %#06x，实际 %#06x", theme.cardBG, uint32(got))
	}

	// 换成浅色配色重画一遍：同一段文字应当用浅色主题的颜色
	lw.mu.Lock()
	lw.theme = lightSet
	lw.mu.Unlock()
	lw.renderAll()
	if got := editCharColorAt(edit, 10, 12); got != lightSet.danger {
		t.Errorf("浅色主题下错误级别词应为 %#06x，实际 %#06x", lightSet.danger, got)
	}

	lw.mu.Lock()
	lw.theme = theme
	lw.mu.Unlock()
	lw.renderAll()
}

// editCharColorAt 读回 [start, end) 这段字符的颜色（EM_GETCHARFORMAT 取选区格式）。
func editCharColorAt(edit windows.Handle, start, end uintptr) uint32 {
	procSendMessageW.Call(uintptr(edit), emSetSel, start, end)
	cf := charFormat2T{CBSize: uint32(unsafe.Sizeof(charFormat2T{}))}
	procSendMessageW.Call(uintptr(edit), emGetCharFormat, scfSelection, uintptr(unsafe.Pointer(&cf)))
	return cf.TextColor
}

// TestWindowProcHandlesSessionEnd 消息层面的回归：WM_QUERYENDSESSION 必须返回 1（同意
// 结束会话）并触发还原，WM_ENDSESSION 在 wParam=0（会话取消）时不得触发。
func TestWindowProcHandlesSessionEnd(t *testing.T) {
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	hwnd := lw.handle()
	defer setSessionEndHandler(nil)

	hits := make(chan struct{}, 4)
	setSessionEndHandler(func() { hits <- struct{}{} })

	if r := wndProc(hwnd, wmQueryEndSession, 0, nil); r != 1 {
		t.Errorf("WM_QUERYENDSESSION 应返回 1（同意结束会话），实际 %d", r)
	}
	select {
	case <-hits:
	default:
		t.Fatal("WM_QUERYENDSESSION 应触发优雅还原")
	}

	// 会话被取消（wParam=0）时不应再次还原
	sessionEndMu.Lock()
	sessionEndLast = time.Time{}
	sessionEndMu.Unlock()
	drain(hits)
	if r := wndProc(hwnd, wmEndSession, 0, nil); r != 0 {
		t.Errorf("WM_ENDSESSION 应返回 0，实际 %d", r)
	}
	if len(hits) != 0 {
		t.Error("WM_ENDSESSION(wParam=0) 表示会话被取消，不应触发还原")
	}

	// 真的结束会话时（wParam=1）要还原
	if r := wndProc(hwnd, wmEndSession, 1, nil); r != 0 {
		t.Errorf("WM_ENDSESSION 应返回 0，实际 %d", r)
	}
	if len(hits) == 0 {
		t.Error("WM_ENDSESSION(wParam=1) 应触发还原")
	}
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// editSelection 返回日志控件当前选区的起止。
func editSelection(edit windows.Handle) (uint32, uint32) {
	var start, end uint32
	procSendMessageW.Call(uintptr(edit), emGetSel,
		uintptr(unsafe.Pointer(&start)), uintptr(unsafe.Pointer(&end)))
	return start, end
}
