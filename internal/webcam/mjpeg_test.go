//go:build linux

package webcam

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"strconv"
	"testing"
)

// splitMJPEG divide uno stream MJPEG (frame JPEG concatenati, come quelli
// salvati da v4l2-ctl --stream-to) nei singoli frame.
func splitMJPEG(b []byte) [][]byte {
	soi := []byte{0xff, 0xd8, 0xff}
	var out [][]byte
	for {
		i := bytes.Index(b, soi)
		if i < 0 {
			return out
		}
		b = b[i:]
		j := bytes.Index(b[2:], soi)
		if j < 0 {
			return append(out, b)
		}
		out = append(out, b[:j+2])
		b = b[j+2:]
	}
}

// refLuma è il piano Y decodificato da image/jpeg.
func refLuma(t testing.TB, data []byte) (int, int, []byte) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, w*h)
	switch m := img.(type) {
	case *image.YCbCr:
		for y := 0; y < h; y++ {
			copy(out[y*w:(y+1)*w], m.Y[y*m.YStride:])
		}
	case *image.Gray:
		for y := 0; y < h; y++ {
			copy(out[y*w:(y+1)*w], m.Pix[y*m.Stride:])
		}
	default:
		t.Fatalf("image/jpeg ha dato %T", img)
	}
	return w, h, out
}

// compareLuma confronta due piani Y: le due IDCT arrotondano in modo
// diverso, quindi è ammesso uno scarto di un livello.
func compareLuma(t testing.TB, name string, got, want []byte) {
	t.Helper()
	diff, maxd := 0, 0
	for i := range want {
		d := int(got[i]) - int(want[i])
		if d < 0 {
			d = -d
		}
		if d != 0 {
			diff++
		}
		maxd = max(maxd, d)
	}
	if maxd > 1 {
		t.Errorf("%s: scarto massimo %d livelli (%d pixel diversi su %d)", name, maxd, diff, len(want))
	}
	t.Logf("%s: %d pixel diversi su %d (%.3f%%), scarto massimo %d", name, diff, len(want), 100*float64(diff)/float64(len(want)), maxd)
}

func streamFrames(t testing.TB) [][]byte {
	path := os.Getenv("WEBCAM_FACE_MJPEG")
	if path == "" {
		t.Skip("WEBCAM_FACE_MJPEG=<stream MJPEG> non impostato")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fr := splitMJPEG(data)
	if len(fr) == 0 {
		t.Fatal("nessun frame JPEG nello stream")
	}
	return fr
}

// TestMJPEGStream decodifica i frame veri di WEBCAM_FACE_MJPEG.
func TestMJPEGStream(t *testing.T) {
	fr := streamFrames(t)
	d := newMJPEGDecoder(4)
	defer d.Close()
	for i, f := range fr {
		if i%max(1, len(fr)/8) != 0 {
			continue
		}
		w, h, want := refLuma(t, f)
		got := make([]byte, w*h)
		if err := d.Decode(f, w, h, got); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		compareLuma(t, "frame "+strconv.Itoa(i), got, want)
	}
}

func BenchmarkMJPEG(b *testing.B) {
	fr := streamFrames(b)
	w, h, _ := refLuma(b, fr[0])
	dst := make([]byte, w*h)
	b.Run("image-jpeg", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := toGray(PixMJPG, fr[i%len(fr)], w, h, 0, dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, par := range []int{1, 2, 4, 8, 16} {
		b.Run("par"+strconv.Itoa(par), func(b *testing.B) {
			d := newMJPEGDecoder(par)
			defer d.Close()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := d.Decode(fr[i%len(fr)], w, h, dst); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// JPEG sintetici: un encoder baseline minimo, con restart, campionamenti
// diversi e tabelle di Huffman con codici da 2 a 16 bit (image/jpeg non
// scrive i restart e usa solo 4:2:0).

type jpegSpec struct {
	w, h   int
	gray   bool
	hy, vy int // campionamento della Y; la crominanza è 1×1
	ri     int // MCU per restart, 0: nessuno
	q      int // passo di quantizzazione di base
}

// testHuff assegna ai simboli (in ordine) codici sempre più lunghi.
type testHuff struct {
	counts [16]byte
	syms   []byte
	code   [256]uint16
	size   [256]uint8
}

func newTestHuff(t *testing.T, syms []byte, perLen []int) *testHuff {
	h := &testHuff{syms: syms}
	k, kraft := 0, 0.0
	for l := 1; l <= 16 && k < len(syms); l++ {
		n := min(perLen[l-1], len(syms)-k)
		if l == 16 {
			n = len(syms) - k
		}
		h.counts[l-1] = byte(n)
		k += n
		kraft += float64(n) / float64(int(1)<<l)
	}
	if k != len(syms) || kraft >= 1 {
		t.Fatalf("tabella di prova non valida: %d simboli su %d, Kraft %.3f", k, len(syms), kraft)
	}
	code, k := uint16(0), 0
	for l := 1; l <= 16; l++ {
		for i := 0; i < int(h.counts[l-1]); i++ {
			h.code[syms[k]], h.size[syms[k]] = code, uint8(l)
			code++
			k++
		}
		code <<= 1
	}
	return h
}

type bitWriter struct {
	out  []byte
	acc  uint32
	nacc uint
}

func (b *bitWriter) bits(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		b.acc = b.acc<<1 | (v>>uint(i))&1
		b.nacc++
		if b.nacc == 8 {
			c := byte(b.acc)
			b.out = append(b.out, c)
			if c == 0xff {
				b.out = append(b.out, 0)
			}
			b.acc, b.nacc = 0, 0
		}
	}
}

// pad completa il byte con bit a uno, come vuole lo standard.
func (b *bitWriter) pad() {
	for b.nacc != 0 {
		b.bits(1, 1)
	}
}

func bitLen(v int) uint {
	if v < 0 {
		v = -v
	}
	n := uint(0)
	for v != 0 {
		n++
		v >>= 1
	}
	return n
}

func (b *bitWriter) value(v int, s uint) {
	if v < 0 {
		v--
	}
	b.bits(uint32(v)&(1<<s-1), s)
}

func testSample(c, x, y int) float64 {
	switch c {
	case 0:
		v := 128 + 70*math.Sin(float64(x)*0.21)*math.Cos(float64(y)*0.17) + float64((x*y*31)%23) - 11
		if (x/5+y/3)%2 == 0 {
			v += 35
		}
		return v
	case 1:
		return 128 + 50*math.Sin(float64(x+y)*0.1)
	}
	return 128 - 40*math.Cos(float64(x-y)*0.13)
}

func encodeTestJPEG(t *testing.T, sp jpegSpec) []byte {
	nc, hy, vy := 3, sp.hy, sp.vy
	if sp.gray {
		nc, hy, vy = 1, 1, 1
	}
	var qt [2][64]int // zig-zag
	for k := 0; k < 64; k++ {
		qt[0][k] = sp.q + k/8
		qt[1][k] = 2*sp.q + k/4
	}
	dcSyms := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	acSyms := []byte{0x00}
	for s := 1; s <= 10; s++ {
		for r := 0; r < 16; r++ {
			acSyms = append(acSyms, byte(r<<4|s))
		}
	}
	acSyms = append(acSyms, 0xf0)
	dcT := newTestHuff(t, dcSyms, []int{0, 2, 0, 4, 0, 5, 0, 0, 0, 0, 0, 1})
	acT := newTestHuff(t, acSyms, []int{0, 1, 1, 2, 3, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22, 0})

	be16 := func(b []byte, v int) []byte { return append(b, byte(v>>8), byte(v)) }
	out := []byte{0xff, 0xd8}
	nq := min(nc, 2)
	out = be16(append(out, 0xff, 0xdb), 2+65*nq)
	for i := 0; i < nq; i++ {
		out = append(out, byte(i))
		for k := 0; k < 64; k++ {
			out = append(out, byte(qt[i][k]))
		}
	}
	out = be16(append(out, 0xff, 0xc0), 8+3*nc)
	out = append(out, 8)
	out = be16(be16(out, sp.h), sp.w)
	out = append(out, byte(nc), 1, byte(hy<<4|vy), 0)
	if nc == 3 {
		out = append(out, 2, 0x11, 1, 3, 0x11, 1)
	}
	out = be16(append(out, 0xff, 0xc4), 2+2*17+len(dcSyms)+len(acSyms))
	out = append(append(append(out, 0x00), dcT.counts[:]...), dcSyms...)
	out = append(append(append(out, 0x10), acT.counts[:]...), acSyms...)
	if sp.ri > 0 {
		out = be16(be16(append(out, 0xff, 0xdd), 4), sp.ri)
	}
	out = be16(append(out, 0xff, 0xda), 6+2*nc)
	out = append(out, byte(nc))
	for i := 0; i < nc; i++ {
		out = append(out, byte(i+1), 0x00)
	}
	out = append(out, 0, 63, 0)

	mcuW, mcuH := 8*hy, 8*vy
	mx, my := (sp.w+mcuW-1)/mcuW, (sp.h+mcuH-1)/mcuH
	var bw bitWriter
	var pred [3]int
	var blk [64]float64
	block := func(c int, x0, y0, step int) {
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				px, py := min((x0+x)*step, sp.w-1), min((y0+y)*step, sp.h-1)
				blk[y*8+x] = math.Max(0, math.Min(255, math.Round(testSample(c, px, py)))) - 128
			}
		}
		q := &qt[min(c, 1)]
		var coef [64]int
		for k := 0; k < 64; k++ {
			u, v := int(unzig[k])%8, int(unzig[k])/8
			sum := 0.0
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					sum += blk[y*8+x] * math.Cos(float64(2*x+1)*float64(u)*math.Pi/16) * math.Cos(float64(2*y+1)*float64(v)*math.Pi/16)
				}
			}
			cu, cv := 1.0, 1.0
			if u == 0 {
				cu = 1 / math.Sqrt2
			}
			if v == 0 {
				cv = 1 / math.Sqrt2
			}
			coef[k] = max(-1023, min(1023, int(math.Round(sum*cu*cv/4/float64(q[k])))))
		}
		diff := coef[0] - pred[c]
		pred[c] = coef[0]
		s := bitLen(diff)
		bw.bits(uint32(dcT.code[s]), uint(dcT.size[s]))
		bw.value(diff, s)
		run := 0
		for k := 1; k < 64; k++ {
			if coef[k] == 0 {
				run++
				continue
			}
			for ; run > 15; run -= 16 {
				bw.bits(uint32(acT.code[0xf0]), uint(acT.size[0xf0]))
			}
			s := bitLen(coef[k])
			sym := byte(run<<4) | byte(s)
			bw.bits(uint32(acT.code[sym]), uint(acT.size[sym]))
			bw.value(coef[k], s)
			run = 0
		}
		if run > 0 {
			bw.bits(uint32(acT.code[0]), uint(acT.size[0]))
		}
	}
	for m := 0; m < mx*my; m++ {
		if sp.ri > 0 && m > 0 && m%sp.ri == 0 {
			bw.pad()
			bw.out = append(bw.out, 0xff, byte(0xd0+(m/sp.ri-1)%8))
			pred = [3]int{}
		}
		x, y := m%mx, m/mx
		for by := 0; by < vy; by++ {
			for bx := 0; bx < hy; bx++ {
				block(0, x*mcuW+bx*8, y*mcuH+by*8, 1)
			}
		}
		if nc == 3 { // crominanza: un campione ogni hy×vy pixel
			block(1, x*8, y*8, max(hy, vy))
			block(2, x*8, y*8, max(hy, vy))
		}
	}
	bw.pad()
	return append(append(out, bw.out...), 0xff, 0xd9)
}

func TestMJPEGSynthetic(t *testing.T) {
	for _, sp := range []jpegSpec{
		{w: 96, h: 40, hy: 2, vy: 1, ri: 3, q: 4},  // 4:2:2 come la C925e
		{w: 101, h: 37, hy: 2, vy: 1, ri: 1, q: 3}, // bordi parziali, restart a ogni MCU
		{w: 77, h: 45, hy: 2, vy: 2, ri: 0, q: 5},  // 4:2:0 senza restart
		{w: 64, h: 24, hy: 1, vy: 1, ri: 5, q: 2},  // 4:4:4
		{w: 50, h: 30, gray: true, ri: 4, q: 3},    // una sola componente
		{w: 64, h: 32, hy: 2, vy: 1, ri: 2, q: 1},  // coefficienti grandi
	} {
		data := encodeTestJPEG(t, sp)
		w, h, want := refLuma(t, data)
		if w != sp.w || h != sp.h {
			t.Fatalf("image/jpeg: %d×%d invece di %d×%d", w, h, sp.w, sp.h)
		}
		for _, par := range []int{1, 3} {
			d := newMJPEGDecoder(par)
			got := make([]byte, w*h)
			for i := range got {
				got[i] = 0x5a // ogni pixel deve essere riscritto
			}
			if err := d.Decode(data, w, h, got); err != nil {
				t.Fatalf("%+v par %d: %v", sp, par, err)
			}
			compareLuma(t, fmt.Sprintf("%dx%d %d:%d gray=%v ri=%d q=%d par=%d", sp.w, sp.h, sp.hy, sp.vy, sp.gray, sp.ri, sp.q, par), got, want)
			if sp.ri > 0 {
				segs := int(d.lastSegs.Load())
				mcus := ((sp.w + 8*max(sp.hy, 1) - 1) / (8 * max(sp.hy, 1))) * ((sp.h + 8*max(sp.vy, 1) - 1) / (8 * max(sp.vy, 1)))
				if want := (mcus + sp.ri - 1) / sp.ri; segs != want {
					t.Errorf("%d tratti, attesi %d", segs, want)
				}
			}
			d.Close()
		}
	}
}

func TestMJPEGNoAllocs(t *testing.T) {
	data := encodeTestJPEG(t, jpegSpec{w: 128, h: 64, hy: 2, vy: 1, ri: 2, q: 3})
	d := newMJPEGDecoder(4)
	defer d.Close()
	dst := make([]byte, 128*64)
	if err := d.Decode(data, 128, 64, dst); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(50, func() { _ = d.Decode(data, 128, 64, dst) }); n != 0 {
		t.Fatalf("%.1f allocazioni per frame, attese 0", n)
	}
}

// I frame rovinati devono dare un errore, mai un panic né pixel fuori dal
// buffer; i JPEG che il decoder non gestisce passano a image/jpeg.
func TestMJPEGBadInput(t *testing.T) {
	const w, h = 96, 40
	data := encodeTestJPEG(t, jpegSpec{w: w, h: h, hy: 2, vy: 1, ri: 3, q: 4})
	d := newMJPEGDecoder(3)
	defer d.Close()
	dst := make([]byte, w*h)
	for _, n := range []int{0, 3, 20, 200, len(data) / 2, len(data) - 2} {
		if err := d.Decode(data[:n], w, h, dst); err == nil {
			t.Errorf("frame troncato a %d byte accettato", n)
		}
	}
	if err := d.Decode(data, w+8, h, dst); err == nil {
		t.Error("dimensioni diverse accettate")
	}
	// byte casuali cambiati: errore o immagine sbagliata, ma niente panic
	rnd := uint32(1)
	bad := make([]byte, len(data))
	for i := 0; i < 2000; i++ {
		copy(bad, data)
		for k := 0; k < 1+i%4; k++ {
			rnd = rnd*1664525 + 1013904223
			bad[int(rnd>>8)%len(bad)] = byte(rnd >> 24)
		}
		_ = d.Decode(bad, w, h, dst)
	}
	// SOF progressivo → image/jpeg
	prog := append([]byte(nil), data...)
	i := bytes.Index(prog, []byte{0xff, 0xc0})
	prog[i+1] = 0xc2
	if err := d.Decode(prog, w, h, dst); !errors.Is(err, errUnsupportedJPEG) {
		t.Fatalf("JPEG progressivo: %v, atteso errUnsupportedJPEG", err)
	}
	c := NewConverter(&CamInfo{PixFmt: PixMJPG, W: w, H: h}, 2)
	defer c.Close()
	if err := c.Gray(data, dst); err != nil || c.fallback.Load() {
		t.Fatalf("decoder veloce: %v, ripiego %v", err, c.fallback.Load())
	}
	_ = c.Gray(prog, dst)
	if !c.fallback.Load() {
		t.Fatal("JPEG progressivo non passato a image/jpeg")
	}
}

// FuzzMJPEG: qualsiasi frame deve dare un errore o un'immagine, mai un
// panic. go test -run '^$' -fuzz FuzzMJPEG ./internal/webcam
func FuzzMJPEG(f *testing.F) {
	t := &testing.T{}
	for _, sp := range []jpegSpec{
		{w: 48, h: 16, hy: 2, vy: 1, ri: 1, q: 4},
		{w: 24, h: 24, hy: 2, vy: 2, q: 6},
		{w: 16, h: 8, gray: true, ri: 1, q: 2},
	} {
		f.Add(encodeTestJPEG(t, sp), uint8(sp.w), uint8(sp.h))
	}
	d := newMJPEGDecoder(2)
	f.Fuzz(func(t *testing.T, data []byte, w, h uint8) {
		dst := make([]byte, int(w)*int(h))
		_ = d.Decode(data, int(w), int(h), dst)
	})
}
