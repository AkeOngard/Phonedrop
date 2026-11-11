// mkico สร้าง assets/icon.png (512x512) และ assets/icon.ico (16/32/48/256)
// วาดเองทั้งหมด: พื้นสีน้ำเงินมุมมน + ลูกศรลงถาด
//
//	go run ./tools/mkico
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const ss = 4 // supersampling ต่อแกน

func inRoundRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x >= x1 || y < y0 || y >= y1 {
		return false
	}
	cx := math.Min(math.Max(x, x0+r), x1-r)
	cy := math.Min(math.Max(y, y0+r), y1-r)
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

func inTri(x, y, ax, ay, bx, by, cx, cy float64) bool {
	s := func(px, py, qx, qy float64) float64 { return (x-qx)*(py-qy) - (px-qx)*(y-qy) }
	d1, d2, d3 := s(ax, ay, bx, by), s(bx, by, cx, cy), s(cx, cy, ax, ay)
	return !((d1 < 0 || d2 < 0 || d3 < 0) && (d1 > 0 || d2 > 0 || d3 > 0))
}

// พิกัดอ้างอิงบนผืน 512x512
func inGlyph(x, y float64) bool {
	return inRoundRect(x, y, 230, 104, 282, 280, 14) || // ก้านลูกศร
		inTri(x, y, 160, 244, 352, 244, 256, 352) || // หัวลูกศร
		inRoundRect(x, y, 128, 376, 384, 420, 16) || // ก้นถาด
		inRoundRect(x, y, 128, 312, 172, 420, 16) || // ขอบซ้าย
		inRoundRect(x, y, 340, 312, 384, 420, 16) // ขอบขวา
}

// render วาดไอคอนขนาด n พิกเซลโดยตรง (คมกว่าการย่อภาพ)
func render(n int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	scale := 512 / float64(n)
	bg := [3]float64{37, 99, 235}
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var inBg, inFg int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * scale
					y := (float64(py) + (float64(sy)+0.5)/ss) * scale
					if inRoundRect(x, y, 16, 16, 496, 496, 108) {
						inBg++
						if inGlyph(x, y) {
							inFg++
						}
					}
				}
			}
			t := 0.0
			if inBg > 0 {
				t = float64(inFg) / float64(inBg)
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(bg[0] + (255-bg[0])*t)),
				G: uint8(math.Round(bg[1] + (255-bg[1])*t)),
				B: uint8(math.Round(bg[2] + (255-bg[2])*t)),
				A: uint8(255 * inBg / (ss * ss)),
			})
		}
	}
	return img
}

func pngBytes(img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// bmpEntry: รูปแบบ DIB 32 บิตสำหรับไอคอนขนาดเล็ก (เข้ากันได้กับ Windows ทุกรุ่น)
func bmpEntry(img *image.NRGBA) []byte {
	n := img.Bounds().Dx()
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, struct {
		Size          uint32
		Width, Height int32
		Planes, Bits  uint16
		Compression   uint32
		ImageSize     uint32
		XPPM, YPPM    int32
		Used, Imp     uint32
	}{Size: 40, Width: int32(n), Height: int32(n * 2), Planes: 1, Bits: 32})
	for y := n - 1; y >= 0; y-- { // ล่างขึ้นบน
		for x := 0; x < n; x++ {
			c := img.NRGBAAt(x, y)
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	b.Write(make([]byte, ((n+31)/32*4)*n)) // AND mask ว่าง (ใช้ alpha แทน)
	return b.Bytes()
}

func main() {
	if err := os.MkdirAll("assets", 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile("assets/icon.png", pngBytes(render(512)), 0o644); err != nil {
		panic(err)
	}

	sizes := []int{16, 32, 48, 256}
	var entries [][]byte
	for _, n := range sizes {
		if n >= 256 {
			entries = append(entries, pngBytes(render(n)))
		} else {
			entries = append(entries, bmpEntry(render(n)))
		}
	}
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, n := range sizes {
		ico.Write([]byte{byte(n), byte(n), 0, 0}) // 256 เขียนเป็น 0 ตามสเปก
		binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
		binary.Write(&ico, binary.LittleEndian, []uint32{uint32(len(entries[i])), uint32(offset)})
		offset += len(entries[i])
	}
	for _, e := range entries {
		ico.Write(e)
	}
	if err := os.WriteFile("assets/icon.ico", ico.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
