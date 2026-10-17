// genicon 生成 internal/tray/icon.ico（圆角蓝底 + 白帆黄船的小帆船，16/32/48 三档尺寸）。
//
// 用法:
//
//	go run ./tools/genicon internal/tray/icon.ico   // 生成 ICO
//	go run ./tools/genicon preview.png              // 生成 PNG 预览（便于查看效果）
package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"strings"
)

func main() {
	path := "internal/tray/icon.ico"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	// PNG 预览模式
	if strings.HasSuffix(strings.ToLower(path), ".png") {
		size := 256
		if len(os.Args) > 2 {
			fmt.Sscanf(os.Args[2], "%d", &size)
		}
		img := render(size)
		f, err := os.Create(path)
		if err != nil {
			die(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			die(err)
		}
		fmt.Println("已生成预览", path)
		return
	}

	// ICO 模式
	sizes := []int{16, 32, 48}
	layers := make([][]byte, 0, len(sizes))
	for _, s := range sizes {
		layers = append(layers, encodeBMP(render(s)))
	}
	if err := os.WriteFile(path, buildICO(layers, sizes), 0o644); err != nil {
		die(err)
	}
	fmt.Printf("已生成 %s（%v）\n", path, sizes)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "失败:", err)
	os.Exit(1)
}

// render 以 4 倍超采样渲染 s x s 图标。
func render(s int) *image.NRGBA {
	const ss = 4
	n := s * ss
	buf := make([]float64, n*n*4)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			r, g, b, a := pixel(float64(x)+0.5, float64(y)+0.5, float64(n))
			i := (y*n + x) * 4
			buf[i], buf[i+1], buf[i+2], buf[i+3] = r, g, b, a
		}
	}
	img := image.NewNRGBA(image.Rect(0, 0, s, s))
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			var sr, sg, sb, sa float64
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					i := ((y*ss+dy)*n + (x*ss + dx)) * 4
					a := buf[i+3]
					sr += buf[i] * a
					sg += buf[i+1] * a
					sb += buf[i+2] * a
					sa += a
				}
			}
			off := (y*s + x) * 4
			if sa > 0 {
				img.Pix[off] = byte(math.Round(255 * sr / sa))
				img.Pix[off+1] = byte(math.Round(255 * sg / sa))
				img.Pix[off+2] = byte(math.Round(255 * sb / sa))
				img.Pix[off+3] = byte(math.Round(255 * sa / float64(ss*ss)))
			}
		}
	}
	return img
}

// pixel 返回超采样坐标 (x,y) 处的颜色（直通 alpha）。
// 图案：圆角蓝底（上浅下深，似海天）+ 白色帆 + 黄色船体。
func pixel(x, y, n float64) (r, g, b, a float64) {
	inset := n * 0.055
	radius := n * 0.23
	minX, minY, maxX, maxY := inset, inset, n-inset, n-inset
	if !inRoundRect(x, y, minX, minY, maxX, maxY, radius) {
		return 0, 0, 0, 0
	}
	u, v := x/n, y/n

	// 背景：垂直渐变蓝（上 #3E7BDF → 下 #11398F）
	t := clamp(v, 0, 1)
	r = lerp(0.243, 0.067, t)
	g = lerp(0.482, 0.224, t)
	b = lerp(0.875, 0.561, t)
	a = 1

	// 船体：黄色，底边略呈弧形
	hull := [][2]float64{
		{0.20, 0.70}, {0.80, 0.70}, {0.72, 0.815}, {0.58, 0.86}, {0.42, 0.86}, {0.28, 0.815},
	}
	if inPolygon(u, v, hull) {
		r, g, b = 1.0, 0.815, 0.27 // #FFD045
	}

	// 桅杆：白色细柱
	if inRect(u, v, 0.485, 0.14, 0.515, 0.70) {
		r, g, b = 0.98, 0.99, 1.0
	}
	// 右主帆（白）
	sailR := [][2]float64{{0.53, 0.175}, {0.53, 0.685}, {0.80, 0.685}}
	if inPolygon(u, v, sailR) {
		r, g, b = 0.98, 0.99, 1.0
	}
	// 左前帆（白，稍小）
	sailL := [][2]float64{{0.47, 0.235}, {0.47, 0.685}, {0.21, 0.685}}
	if inPolygon(u, v, sailL) {
		r, g, b = 0.98, 0.99, 1.0
	}
	return
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func inRect(x, y, x0, y0, x1, y1 float64) bool {
	return x >= x0 && x <= x1 && y >= y0 && y <= y1
}

func inRoundRect(x, y, minX, minY, maxX, maxY, r float64) bool {
	if x < minX || x > maxX || y < minY || y > maxY {
		return false
	}
	cx := clamp(x, minX+r, maxX-r)
	cy := clamp(y, minY+r, maxY-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func inPolygon(x, y float64, poly [][2]float64) bool {
	inside := false
	j := len(poly) - 1
	for i := 0; i < len(poly); i++ {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			inside = !inside
		}
		j = i
	}
	return inside
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// encodeBMP 将 NRGBA 图像编码为 32bpp BMP（ICO 内嵌格式：含 AND 掩码）。
func encodeBMP(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	maskRow := ((w + 31) / 32) * 4
	header := make([]byte, 40)
	binary.LittleEndian.PutUint32(header[0:], 40)          // biSize
	binary.LittleEndian.PutUint32(header[4:], uint32(w))   // biWidth
	binary.LittleEndian.PutUint32(header[8:], uint32(h*2)) // biHeight（XOR + AND）
	binary.LittleEndian.PutUint16(header[12:], 1)          // biPlanes
	binary.LittleEndian.PutUint16(header[14:], 32)         // biBitCount
	binary.LittleEndian.PutUint32(header[16:], 0)          // biCompression = BI_RGB
	binary.LittleEndian.PutUint32(header[20:], uint32(w*h*4+maskRow*h))
	binary.LittleEndian.PutUint32(header[24:], 0) // biXPelsPerMeter
	binary.LittleEndian.PutUint32(header[28:], 0) // biYPelsPerMeter
	binary.LittleEndian.PutUint32(header[32:], 0) // biClrUsed
	binary.LittleEndian.PutUint32(header[36:], 0) // biClrImportant

	out := make([]byte, 0, len(header)+w*h*4+maskRow*h)
	out = append(out, header...)
	// XOR 数据：自下而上，BGRA
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			out = append(out, img.Pix[i+2], img.Pix[i+1], img.Pix[i], img.Pix[i+3])
		}
	}
	// AND 掩码：全 0（不透明，透明度由 BGRA alpha 通道表达）
	out = append(out, make([]byte, maskRow*h)...)
	return out
}

// buildICO 组装 ICO 文件。
func buildICO(layers [][]byte, sizes []int) []byte {
	out := make([]byte, 0, 6+16*len(layers))
	var hdr [6]byte
	binary.LittleEndian.PutUint16(hdr[2:], 1)                   // type = icon
	binary.LittleEndian.PutUint16(hdr[4:], uint16(len(layers))) // count
	out = append(out, hdr[:]...)

	offset := 6 + 16*len(layers)
	for i, data := range layers {
		var e [16]byte
		e[0] = byte(sizes[i])
		e[1] = byte(sizes[i])
		e[4], e[5] = 1, 0  // planes
		e[6], e[7] = 32, 0 // bitcount
		binary.LittleEndian.PutUint32(e[8:], uint32(len(data)))
		binary.LittleEndian.PutUint32(e[12:], uint32(offset))
		out = append(out, e[:]...)
		offset += len(data)
	}
	for _, data := range layers {
		out = append(out, data...)
	}
	return out
}
