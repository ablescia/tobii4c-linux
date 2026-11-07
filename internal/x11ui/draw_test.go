package x11ui

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Il testo disegnato con la cache dei glifi è identico a quello senza.
func TestGlyphCacheIdentical(t *testing.T) {
	for _, ttf := range [][]byte{TTFRegular, TTFMono} {
		f, err := NewFont(ttf, 13.7)
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := opentype.Parse(ttf)
		plain, _ := opentype.NewFace(parsed, &opentype.FaceOptions{Size: 13.7, DPI: 72, Hinting: font.HintingFull})
		s := "Pupilla −1,25 mm (0,503; 0,497) ÀÈ fi WAVE"
		for pass := 0; pass < 2; pass++ { // la seconda volta i glifi vengono dalla cache
			a := image.NewRGBA(image.Rect(0, 0, 400, 30))
			b := image.NewRGBA(image.Rect(0, 0, 400, 30))
			Text(a, f, 3, 20, s, color.RGBA{255, 255, 255, 255}, Left)
			d := font.Drawer{Dst: b, Src: image.NewUniform(color.RGBA{255, 255, 255, 255}), Face: plain, Dot: fixed.P(3, 20)}
			d.DrawString(s)
			if !bytes.Equal(a.Pix, b.Pix) {
				t.Fatalf("pass %d: cached text differs from plain text", pass)
			}
			if f.Measure(s) != font.MeasureString(plain, s).Ceil() {
				t.Fatalf("measure differs")
			}
		}
	}
}

// Le primitive veloci restano nel riquadro di clip e nell'immagine.
func TestFastPrimitivesClip(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 50, 40))
	clip := image.Rect(10, 10, 30, 30)
	white := color.RGBA{255, 255, 255, 255}
	ThinLine(img, clip, -100, -50, 200, 300, white, 1)
	ThinLine(img, clip, 5, 20, 45, 21, white, 1)
	ThinLine(img, clip, 20, 5, 20.5, 45, white, 1)
	Dot(img, clip, 10, 10, 3, white, 1)
	Dot(img, clip, -5, 100, 3, white, 1) // fuori dall'immagine
	lit := 0
	for y := 0; y < 40; y++ {
		for x := 0; x < 50; x++ {
			if img.Pix[img.PixOffset(x, y)+3] != 0 {
				if !(image.Point{x, y}).In(clip) {
					t.Fatalf("pixel (%d,%d) outside clip", x, y)
				}
				lit++
			}
		}
	}
	if lit < 40 {
		t.Fatalf("only %d pixels drawn", lit)
	}
}
