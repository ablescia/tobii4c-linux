//go:build linux

package webcam

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"testing"
)

// TestMJPEGColor confronta i piani Cb e Cr del decoder con quelli di
// image/jpeg, per i sottocampionamenti delle webcam.
func TestMJPEGColor(t *testing.T) {
	for _, sp := range []jpegSpec{
		{w: 96, h: 40, hy: 2, vy: 1, ri: 3, q: 4},  // 4:2:2 come la C925e
		{w: 101, h: 37, hy: 2, vy: 1, ri: 1, q: 3}, // bordi parziali
		{w: 77, h: 45, hy: 2, vy: 2, ri: 0, q: 5},  // 4:2:0 senza restart
		{w: 64, h: 24, hy: 1, vy: 1, ri: 5, q: 2},  // 4:4:4
		{w: 50, h: 30, gray: true, ri: 4, q: 3},    // una sola componente
	} {
		data := encodeTestJPEG(t, sp)
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("%dx%d %d:%d gray=%v", sp.w, sp.h, sp.hy, sp.vy, sp.gray)
		for _, par := range []int{1, 3} {
			d := newMJPEGDecoder(par)
			f := NewFrame(sp.w, sp.h, true)
			cw, ch, err := d.DecodeColor(data, sp.w, sp.h, f.Y, f.Cb, f.Cr)
			d.Close()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			m, ok := img.(*image.YCbCr)
			if !ok {
				if cw != 0 || ch != 0 {
					t.Fatalf("%s: crominanza %d×%d in un JPEG in scala di grigi", name, cw, ch)
				}
				continue
			}
			wcw, wch := chromaSize(m)
			if cw != wcw || ch != wch {
				t.Fatalf("%s: crominanza %d×%d, attesa %d×%d", name, cw, ch, wcw, wch)
			}
			for k, pl := range [2][]byte{m.Cb, m.Cr} {
				want := make([]byte, cw*ch)
				for y := 0; y < ch; y++ {
					copy(want[y*cw:(y+1)*cw], pl[y*m.CStride:])
				}
				got := [2][]byte{f.Cb, f.Cr}[k][:cw*ch]
				compareLuma(t, fmt.Sprintf("%s %s par=%d", name, [2]string{"Cb", "Cr"}[k], par), got, want)
			}
		}
	}
}

func TestMJPEGColorNoAllocs(t *testing.T) {
	data := encodeTestJPEG(t, jpegSpec{w: 128, h: 64, hy: 2, vy: 1, ri: 2, q: 3})
	c := NewConverter(&CamInfo{PixFmt: PixMJPG, W: 128, H: 64}, 4)
	defer c.Close()
	f := NewFrame(128, 64, true)
	if err := c.Convert(data, f, true); err != nil || !f.Color() {
		t.Fatalf("%v, colore %v", err, f.Color())
	}
	if n := testing.AllocsPerRun(50, func() { _ = c.Convert(data, f, true) }); n != 0 {
		t.Fatalf("%.1f allocazioni per frame, attese 0", n)
	}
}

func TestYUYVColor(t *testing.T) {
	const w, h = 6, 2
	data := make([]byte, 2*w*h)
	for i := range data {
		data[i] = byte(i * 7)
	}
	c := NewConverter(&CamInfo{PixFmt: PixYUYV, W: w, H: h}, 1)
	f := NewFrame(w, h, true)
	if err := c.Convert(data, f, true); err != nil {
		t.Fatal(err)
	}
	if f.CW != 3 || f.CH != 2 {
		t.Fatalf("crominanza %d×%d", f.CW, f.CH)
	}
	for y := 0; y < h; y++ {
		for k := 0; k < 3; k++ {
			o := y*2*w + 4*k
			if f.Y[y*w+2*k] != data[o] || f.Y[y*w+2*k+1] != data[o+2] || f.Cb[y*3+k] != data[o+1] || f.Cr[y*3+k] != data[o+3] {
				t.Fatalf("pixel %d,%d sbagliato", 2*k, y)
			}
		}
	}
}

func TestConverterNote(t *testing.T) {
	data := encodeTestJPEG(t, jpegSpec{w: 32, h: 16, hy: 2, vy: 1, ri: 1, q: 3})
	c := NewConverter(&CamInfo{PixFmt: PixMJPG, W: 32, H: 16}, 2)
	defer c.Close()
	f := NewFrame(32, 16, true)
	if err := c.Gray(data, f.Y); err != nil || c.Note() != "JPEG → Y · 2 goroutine" {
		t.Errorf("solo luminanza: %q, %v", c.Note(), err)
	}
	if err := c.Convert(data, f, true); err != nil || c.Note() != "JPEG → Y, Cb e Cr · 2 goroutine" {
		t.Errorf("a colori: %q, %v", c.Note(), err)
	}
	y := NewConverter(&CamInfo{PixFmt: PixYUYV, W: 4, H: 2}, 1)
	if y.Note() != "solo la Y" {
		t.Errorf("YUYV: %q", y.Note())
	}
}
