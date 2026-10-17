//go:build windows

package main

import (
	"math"
	"testing"
)

// 本文件是「配色契约」：把配色设计规则变成可执行的断言，避免以后随手改一个色值就破坏
// 可读性或色调一致性。规则见 uiTheme 的注释：
//  1. 深浅两套共用色相，只差明度；
//  2. 中性色统一为冷灰（同一色相）；
//  3. 对比度按 WCAG 分级（正文 ≥7:1、次要 ≥4.5:1、弱化文字与状态点 ≥3.5:1）。

// relLuminance 返回颜色的相对亮度（WCAG 2.x 定义）。入参是 COLORREF（0x00BBGGRR）。
func relLuminance(c uint32) float64 {
	ch := func(v uint32) float64 {
		s := float64(v&0xFF) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c) + 0.7152*ch(c>>8) + 0.0722*ch(c>>16)
}

// contrastRatio 返回两个颜色的 WCAG 对比度（1:1 ~ 21:1）。
func contrastRatio(a, b uint32) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// hueOf 返回颜色的色相角（0-360°）与饱和度（0-1）。
func hueOf(c uint32) (hue, sat float64) {
	r := float64(c&0xFF) / 255
	g := float64((c>>8)&0xFF) / 255
	b := float64((c>>16)&0xFF) / 255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	d := mx - mn
	if d == 0 {
		return 0, 0
	}
	switch mx {
	case r:
		hue = math.Mod((g-b)/d, 6)
	case g:
		hue = (b-r)/d + 2
	default:
		hue = (r-g)/d + 4
	}
	hue *= 60
	if hue < 0 {
		hue += 360
	}
	return hue, d / (1 - math.Abs(2*((mx+mn)/2)-1))
}

// TestPaletteContrast 对比度必须达标：文字可读是硬指标，不能靠「看起来还行」。
func TestPaletteContrast(t *testing.T) {
	themes := []struct {
		name string
		th   uiTheme
	}{{"深色", themeDark}, {"浅色", themeLight}}

	for _, tc := range themes {
		// 日志正文压在卡片上；状态条文字压在窗口底（模糊材质）上——两个面都要够。
		surfaces := []struct {
			name string
			c    uint32
		}{{"日志卡片", tc.th.cardBG}, {"窗口底", tc.th.bg}}

		for _, s := range surfaces {
			for _, want := range []struct {
				name string
				c    uint32
				min  float64
			}{
				{"正文", tc.th.text, 7},
				{"次要文字", tc.th.muted, 4.5},
				{"弱化文字", tc.th.faint, 3.5},
				{"标题强调色", tc.th.accent, 4},
				{"错误红（日志文本）", tc.th.danger, 4.5},
			} {
				if cr := contrastRatio(want.c, s.c); cr < want.min {
					t.Errorf("%s 主题：%s 对%s的对比度 %.2f:1，要求 ≥%.1f:1",
						tc.name, want.name, s.name, cr, want.min)
				}
			}
		}

		// 状态点/状态词是图形与短文字：≥3.5:1
		for _, st := range []struct {
			name string
			c    uint32
		}{{"正常(绿)", tc.th.ok}, {"关闭(灰)", tc.th.off}, {"降级(橙)", tc.th.warn}, {"错误(红)", tc.th.danger}} {
			for _, bg := range []struct {
				name string
				c    uint32
			}{{"卡片", tc.th.cardBG}, {"窗口底", tc.th.bg}} {
				if cr := contrastRatio(st.c, bg.c); cr < 3.5 {
					t.Errorf("%s 主题：%s 状态色对%s的对比度 %.2f:1，要求 ≥3.5:1",
						tc.name, st.name, bg.name, cr)
				}
			}
		}

		// 卡片与窗口底要能看出「一层」：差异太小就白做分层了
		// （浅色下白卡片靠描边定义边界，因此这里阈值放宽到 1.10）
		if cr := contrastRatio(tc.th.cardBG, tc.th.bg); cr < 1.10 {
			t.Errorf("%s 主题：卡片与窗口底的层次差只有 %.2f:1，要求 ≥1.10:1", tc.name, cr)
		}
		// 发丝线必须比底色更「显」，否则分隔线与描边等于没画
		if cr := contrastRatio(tc.th.hairline, tc.th.bg); cr < 1.15 {
			t.Errorf("%s 主题：发丝线与窗口底的对比度只有 %.2f:1，要求 ≥1.15:1", tc.name, cr)
		}
	}
}

// TestPaletteHueHarmony 色调和谐：深浅两套是「同一个设计的两种明度」，不是两种风格。
func TestPaletteHueHarmony(t *testing.T) {
	// 语义色：深浅两套必须同色相（±8°），且深色那套更亮（压在暗底上才看得清）
	for _, g := range []struct {
		name        string
		light, dark uint32
	}{
		{"强调色", themeLight.accent, themeDark.accent},
		{"正常绿", themeLight.ok, themeDark.ok},
		{"降级橙", themeLight.warn, themeDark.warn},
		{"错误红", themeLight.danger, themeDark.danger},
		{"状态灰", themeLight.off, themeDark.off},
		{"次要文字", themeLight.muted, themeDark.muted},
	} {
		hl, _ := hueOf(g.light)
		hd, _ := hueOf(g.dark)
		if d := math.Abs(hl - hd); d > 8 {
			t.Errorf("%s 的深浅两套色相不一致：浅色 %.0f° / 深色 %.0f°（差 %.0f°，要求 ≤8°）",
				g.name, hl, hd, d)
		}
		if relLuminance(g.dark) <= relLuminance(g.light) {
			t.Errorf("%s 的深色变体应比浅色变体更亮（深色主题要抬高亮度），实际 %.3f vs %.3f",
				g.name, relLuminance(g.dark), relLuminance(g.light))
		}
	}

	// 中性色（表面/线条/三级文字）统一为同一种冷灰：不能有的偏蓝有的偏暖
	for _, n := range []struct {
		name        string
		light, dark uint32
	}{
		{"窗口底", themeLight.bg, themeDark.bg},
		{"卡片", themeLight.cardBG, themeDark.cardBG},
		{"发丝线", themeLight.hairline, themeDark.hairline},
		{"弱化文字", themeLight.faint, themeDark.faint},
	} {
		for mode, c := range map[string]uint32{"浅色": n.light, "深色": n.dark} {
			h, s := hueOf(c)
			if s < 0.02 {
				continue // 纯灰：无色相可言，自然与任何色相共存
			}
			if math.Abs(h-240) > 8 {
				t.Errorf("%s 的%s不是冷灰：色相 %.0f°（应接近 240°，饱和度 %.0f%%）",
					mode, n.name, h, s*100)
			}
		}
	}
}
