//go:build windows

package main

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 截图工具用到的三个 gdi32 调用（只为测试服务，因此不进产品代码）。
var (
	procSaveDC           = gdi32.NewProc("SaveDC")
	procRestoreDC        = gdi32.NewProc("RestoreDC")
	procSetViewportOrgEx = gdi32.NewProc("SetViewportOrgEx")
)

// TestUISnapshot 是给「做界面」用的自查工具：把整块客户区画到内存位图上存成 PNG，
// 深浅主题、悬停态、轻量提示各出一张，肉眼过一遍配色与间距。
//
// 子控件（日志控件、过滤框）画在自己的窗口 DC 上，因此这里额外请它们各画一遍
// （WM_PRINTCLIENT），截图里才有完整界面。
//
// 默认跳过（写文件不该是测试的副作用），需要时显式打开：
//
//	$env:TONGTU_UI_SNAPSHOT = "$env:TEMP\tongtu-ui"; go test ./cmd/tongtu -run TestUISnapshot -count=1 -v
//
// 截图目录会打印在日志里，直接打开看即可。
func TestUISnapshot(t *testing.T) {
	dir := os.Getenv("TONGTU_UI_SNAPSHOT")
	if dir == "" {
		t.Skip("未设置 TONGTU_UI_SNAPSHOT，跳过界面截图")
	}
	if err := showLogWindow(); err != nil {
		t.Skipf("当前会话无法创建窗口: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建截图目录失败: %v", err)
	}

	setWindowStatus(windowStatus{
		Title: "通途 Tongtu " + Version,
		State: "部分生效",
		Level: stateDegraded,
		Items: []windowStatusItem{
			{Label: "系统代理", On: true, Action: uiIDSys, Tip: uiModeTips[uiIDSys]},
			{Label: "Hosts", On: false, Action: uiIDHosts, Tip: uiModeTips[uiIDHosts]},
			{Label: "无SNI反代", On: true, Action: uiIDMITM, Tip: uiModeTips[uiIDMITM]},
		},
		Detail: []string{"服务组 Steam、GitHub", "活跃隧道 3", "↓1.2 MB ↑340 KB", "系统代理被其他程序接管"},
	})
	emitSnapshotLog()

	lw.mu.Lock()
	hwnd := lw.hwnd
	dark, light := lw.themeDarkSet, lw.themeLightSet
	savedHover, savedOnlyWarn, savedToast := lw.hoverID, lw.onlyWarn, lw.toastMsg
	lw.mu.Unlock()
	defer func() {
		lw.mu.Lock()
		lw.hoverID, lw.onlyWarn, lw.toastMsg = savedHover, savedOnlyWarn, savedToast
		lw.mu.Unlock()
	}()

	var cr rectT
	procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cr)))

	cases := []struct {
		name  string
		setup func()
	}{
		{"light", func() {
			lw.mu.Lock()
			lw.theme, lw.hoverID, lw.onlyWarn, lw.toastMsg = light, "", false, uiToast{}
			lw.mu.Unlock()
		}},
		{"light-hover", func() {
			lw.mu.Lock()
			lw.theme, lw.hoverID, lw.onlyWarn = light, uiIDSys, true
			lw.toastMsg = uiToast{Text: "Hosts 已开启", Style: styleOK, Until: time.Now().Add(time.Minute)}
			lw.mu.Unlock()
		}},
		{"dark", func() {
			lw.mu.Lock()
			lw.theme, lw.hoverID, lw.onlyWarn, lw.toastMsg = dark, "", false, uiToast{}
			lw.mu.Unlock()
		}},
		{"dark-hover", func() {
			lw.mu.Lock()
			lw.theme, lw.hoverID, lw.onlyWarn = dark, uiIDMITM, true
			lw.toastMsg = uiToast{Text: "写入 hosts 需要管理员权限", Style: styleWarn, Until: time.Now().Add(time.Minute)}
			lw.mu.Unlock()
		}},
	}

	for _, tc := range cases {
		tc.setup()
		m := newMemDC(cr.Right, cr.Bottom)
		if m == nil {
			t.Fatal("无法创建内存 DC")
		}
		lw.paintClient(m.dc, cr)
		headerLayoutFor(t, cr.Right, cr.Bottom).paintChildren(m.dc)
		path := filepath.Join(dir, "tongtu-ui-"+tc.name+".png")
		if err := writePNG(path, m.toImage()); err != nil {
			t.Errorf("写 %s 失败: %v", path, err)
		} else {
			t.Logf("已写出 %s（%dx%d）", path, cr.Right, cr.Bottom)
		}
		m.close()
	}
}

// paintChildren 把子控件（过滤框、日志控件）也画到同一个 DC 上（截图用）。
// 真机上它们各自画在自己的窗口 DC 上，这里是手动请它们各自画一遍。
func (l uiLayout) paintChildren(hdc uintptr) {
	for _, c := range []struct {
		hwnd windows.Handle
		at   pointT
	}{
		{lw.filterHandle(), pointT{X: l.Filter.Left, Y: l.Filter.Top}},
		{lw.editHandle(), pointT{X: l.Edit.Left, Y: l.Edit.Top}},
	} {
		if c.hwnd == 0 {
			continue
		}
		save, _, _ := procSaveDC.Call(hdc)
		procSetViewportOrgEx.Call(hdc, uintptr(c.at.X), uintptr(c.at.Y), 0)
		procSendMessageW.Call(uintptr(c.hwnd), wmPrintClient, hdc, 0)
		procRestoreDC.Call(hdc, save)
	}
}

// emitSnapshotLog 造一段有代表性的日志（含警告与错误），截图里的日志区才有内容。
func emitSnapshotLog() {
	h := newWindowHandler(lw, slog.LevelDebug)
	rec := func(level slog.Level, msg string, attrs ...slog.Attr) {
		r := slog.NewRecord(time.Now(), level, msg, 0)
		r.AddAttrs(attrs...)
		_ = h.Handle(context.Background(), r)
	}
	rec(slog.LevelInfo, "已启用系统代理模式", slog.String("listen", "127.0.0.1:19180"),
		slog.String("bypass", "localhost;127.*"))
	rec(slog.LevelInfo, "服务组已加载", slog.String("groups", "[steam github]"), slog.Int("hosts", 27))
	rec(slog.LevelWarn, "写入 hosts 需要管理员权限",
		slog.String("err", "open C:\\Windows\\System32\\drivers\\etc\\hosts: Access is denied."))
	rec(slog.LevelError, "提权后仍未成功", slog.Int("exit", 1))
	rec(slog.LevelInfo, "隧道已建立", slog.String("target", "steamcommunity.com:443"),
		slog.String("via", "23.45.1.9"), slog.String("proto", "TLS1.3"))
}

// toImage 把内存位图读成 image.RGBA（位图像素是 0x00RRGGBB）。
func (m *memDC) toImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, int(m.w), int(m.h)))
	for y := int32(0); y < m.h; y++ {
		for x := int32(0); x < m.w; x++ {
			c := m.pixel(x, y)
			img.SetRGBA(int(x), int(y), color.RGBA{R: byte(c >> 16), G: byte(c >> 8), B: byte(c), A: 0xFF})
		}
	}
	return img
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
