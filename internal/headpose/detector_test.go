package headpose

import (
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/irimg"
)

func TestCropRoundTrip(t *testing.T) {
	c := Crop{CX: 300, CY: 220, Side: 180, Angle: 0.3}
	for _, p := range [][2]float64{{0, 0}, {128, 128}, {255, 17}, {40, 200}} {
		x, y := c.ToImage(p[0], p[1], 256)
		u, v := c.FromImage(x, y, 256)
		if math.Abs(u-p[0]) > 1e-9 || math.Abs(v-p[1]) > 1e-9 {
			t.Fatalf("%v → (%v,%v) → (%v,%v)", p, x, y, u, v)
		}
	}
	if x, y := c.ToImage(128, 128, 256); x != 300 || y != 220 {
		t.Fatalf("centre maps to (%v,%v)", x, y)
	}
	// l'asse u del ritaglio segue l'angolo
	x0, y0 := c.ToImage(128, 128, 256)
	x1, y1 := c.ToImage(228, 128, 256)
	if a := math.Atan2(y1-y0, x1-x0); math.Abs(a-0.3) > 1e-12 {
		t.Fatalf("u axis angle %v", a)
	}
}

func TestSamplerNormalisesContrast(t *testing.T) {
	f := &irimg.Frame{W: 64, H: 64, Pix: make([]byte, 64*64)}
	for i := range f.Pix {
		f.Pix[i] = byte(10 + (i%64)/4) // gradiente scuro 10..25
	}
	s := newSampler(16)
	dst := make([]float32, 16*16*3)
	s.sample(f, Crop{CX: 32, CY: 32, Side: 64}, dst)
	lo, hi := float32(1), float32(0)
	for _, v := range dst {
		lo, hi = min(lo, v), max(hi, v)
	}
	if lo > 0.05 || hi < 0.95 {
		t.Fatalf("contrast not stretched: %v..%v", lo, hi)
	}
}

// loadGray legge un'immagine PNG o JPEG in scala di grigi.
func loadGray(t testing.TB, path string) *irimg.Frame {
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	img, _, err := image.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	f := &irimg.Frame{W: b.Dx(), H: b.Dy(), Pix: make([]byte, b.Dx()*b.Dy())}
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			f.Pix[y*f.W+x] = color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
		}
	}
	return f
}

// drawLandmarks salva un PNG con i landmark sovrapposti all'immagine.
func drawLandmarks(t testing.TB, f *irimg.Frame, lm *Landmarks, c Crop, path string) {
	drawLandmarksZoom(t, f, lm, c, path, image.Rectangle{})
}

// drawLandmarksZoom salva solo il rettangolo zoom (se non vuoto).
func drawLandmarksZoom(t testing.TB, f *irimg.Frame, lm *Landmarks, c Crop, path string, zoom image.Rectangle) {
	img := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
	for i, v := range f.Pix {
		img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = v, v, v, 255
	}
	dot := func(x, y float64, r int, c color.RGBA) {
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				img.Set(int(x)+dx, int(y)+dy, c)
			}
		}
	}
	for i := 0; i < 468; i++ {
		dot(lm[i][0], lm[i][1], 0, color.RGBA{0, 255, 0, 255})
	}
	for _, i := range []int{lmRightEyeOuter, lmRightEyeInner, lmLeftEyeOuter, lmLeftEyeInner, lmNoseTip} {
		dot(lm[i][0], lm[i][1], 2, color.RGBA{255, 0, 0, 255})
	}
	for _, i := range []int{lmRightIris, lmLeftIris} {
		dot(lm[i][0], lm[i][1], 2, color.RGBA{0, 128, 255, 255})
	}
	for k := 0; k < 4; k++ { // bordo del ritaglio
		for s := 0.0; s < 1; s += 0.002 {
			var u, v float64
			switch k {
			case 0:
				u, v = s*256, 0
			case 1:
				u, v = 256, s*256
			case 2:
				u, v = 256-s*256, 256
			case 3:
				u, v = 0, 256-s*256
			}
			x, y := c.ToImage(u, v, 256)
			dot(x, y, 0, color.RGBA{255, 255, 0, 255})
		}
	}
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var out image.Image = img
	if !zoom.Empty() {
		out = img.SubImage(zoom)
	}
	if err := png.Encode(fh, out); err != nil {
		t.Fatal(err)
	}
}

// TestDetectOnImage esegue ricerca e landmark su un'immagine indicata da
// $TOBII4C_FACE_IMAGE e salva il risultato in $TOBII4C_FACE_OUT (PNG).
func TestDetectOnImage(t *testing.T) {
	path := os.Getenv("TOBII4C_FACE_IMAGE")
	if path == "" {
		t.Skip("set TOBII4C_FACE_IMAGE")
	}
	d, err := NewDetector(4)
	if err != nil {
		t.Fatal(err)
	}
	f := loadGray(t, path)
	var lm Landmarks
	t0 := time.Now()
	c, score := d.Search(f, &lm)
	t.Logf("search %v: score %.3f crop %+v", time.Since(t0), score, c)
	if score < minFaceScore {
		t.Fatalf("no face found (best score %.3f)", score)
	}
	r, l := lm[lmRightEyeOuter], lm[lmLeftEyeOuter]
	t.Logf("eye corners R(%.0f,%.0f) L(%.0f,%.0f) nose (%.0f,%.0f) irises R(%.0f,%.0f) L(%.0f,%.0f)",
		r[0], r[1], l[0], l[1], lm[lmNoseTip][0], lm[lmNoseTip][1],
		lm[lmRightIris][0], lm[lmRightIris][1], lm[lmLeftIris][0], lm[lmLeftIris][1])
	if out := os.Getenv("TOBII4C_FACE_OUT"); out != "" {
		drawLandmarks(t, f, &lm, c, out)
	}
}
