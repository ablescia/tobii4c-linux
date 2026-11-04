package nn

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/facemodel"
)

// --- codificatore protobuf minimale per costruire modelli di prova ---------

type pb []byte

func (b pb) varint(v uint64) pb {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}
func (b pb) key(num, wire int) pb       { return b.varint(uint64(num<<3 | wire)) }
func (b pb) bytes(num int, v []byte) pb { return append(b.key(num, 2).varint(uint64(len(v))), v...) }
func (b pb) str(num int, s string) pb   { return b.bytes(num, []byte(s)) }
func (b pb) int(num int, v int64) pb    { return b.key(num, 0).varint(uint64(v)) }

func attrIntsPB(name string, v ...int64) []byte {
	var packed pb
	for _, x := range v {
		packed = packed.varint(uint64(x))
	}
	return pb(nil).str(1, name).bytes(8, packed).int(20, 7)
}
func attrIntPB(name string, v int64) []byte { return pb(nil).str(1, name).int(3, v).int(20, 2) }

func floatTensorPB(name string, dims []int, data []float32) []byte {
	b := pb(nil)
	for _, d := range dims {
		b = b.int(1, int64(d))
	}
	raw := make([]byte, 4*len(data))
	for i, v := range data {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(v))
	}
	return b.int(2, dtFloat).str(8, name).bytes(9, raw)
}

func intTensorPB(name string, data ...int64) []byte {
	b := pb(nil).int(1, int64(len(data))).int(2, dtInt64).str(8, name)
	var packed pb
	for _, v := range data {
		packed = packed.varint(uint64(v))
	}
	return b.bytes(7, packed)
}

func nodePB(op string, in, out []string, attrs ...[]byte) []byte {
	b := pb(nil)
	for _, s := range in {
		b = b.str(1, s)
	}
	for _, s := range out {
		b = b.str(2, s)
	}
	b = b.str(4, op)
	for _, a := range attrs {
		b = b.bytes(5, a)
	}
	return b
}

func randn(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(r.NormFloat64() * 0.5)
	}
	return v
}

// --- riferimento banale (float64, NCHW) ------------------------------------

type ref struct {
	c, h, w int
	d       []float64
}

func (t ref) at(c, y, x int) float64 {
	if y < 0 || x < 0 || y >= t.h || x >= t.w {
		return 0
	}
	return t.d[(c*t.h+y)*t.w+x]
}

func refConv(x ref, w []float32, wc, m, k int, b []float32, s, pt, pl, pb_, pr, group int) ref {
	ho := (x.h+pt+pb_-k)/s + 1
	wo := (x.w+pl+pr-k)/s + 1
	out := ref{m, ho, wo, make([]float64, m*ho*wo)}
	per := m / group
	for o := 0; o < m; o++ {
		g := o / per
		for oy := 0; oy < ho; oy++ {
			for ox := 0; ox < wo; ox++ {
				sum := float64(b[o])
				for ci := 0; ci < wc; ci++ {
					cin := g*wc + ci
					for ky := 0; ky < k; ky++ {
						for kx := 0; kx < k; kx++ {
							sum += float64(w[((o*wc+ci)*k+ky)*k+kx]) * x.at(cin, oy*s-pt+ky, ox*s-pl+kx)
						}
					}
				}
				out.d[(o*ho+oy)*wo+ox] = sum
			}
		}
	}
	return out
}

func refPRelu(x ref, sl []float32) ref {
	out := ref{x.c, x.h, x.w, make([]float64, len(x.d))}
	for i, v := range x.d {
		if v < 0 {
			v *= float64(sl[i/(x.h*x.w)])
		}
		out.d[i] = v
	}
	return out
}

func TestTinyGraphMatchesReference(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	const H, W = 34, 46 // righe abbastanza lunghe per i kernel SIMD (blocchi di 8)
	w1, b1 := randn(r, 4*3*3*3), randn(r, 4)
	s1, s2, s3 := randn(r, 4), randn(r, 4), randn(r, 1)
	wd, bd := randn(r, 4*1*3*3), randn(r, 4)
	wp, bp := randn(r, 4*4), randn(r, 4)
	wh, bh := randn(r, 5*8*2*2), randn(r, 5)

	g := pb(nil)
	g = g.bytes(1, nodePB("Transpose", []string{"in"}, []string{"t"}, attrIntsPB("perm", 0, 3, 1, 2)))
	g = g.bytes(1, nodePB("Conv", []string{"t", "w1", "b1"}, []string{"c1"},
		attrIntsPB("kernel_shape", 3, 3), attrIntsPB("strides", 2, 2), attrIntsPB("pads", 0, 0, 1, 1),
		attrIntsPB("dilations", 1, 1), attrIntPB("group", 1)))
	g = g.bytes(1, nodePB("PRelu", []string{"c1", "s1"}, []string{"p1"}))
	g = g.bytes(1, nodePB("Conv", []string{"p1", "wd", "bd"}, []string{"dw"},
		attrIntsPB("kernel_shape", 3, 3), attrIntsPB("pads", 1, 1, 1, 1), attrIntPB("group", 4)))
	g = g.bytes(1, nodePB("Conv", []string{"dw", "wp", "bp"}, []string{"pw"}, attrIntsPB("kernel_shape", 1, 1)))
	g = g.bytes(1, nodePB("PRelu", []string{"pw", "s2"}, []string{"q"})) // fusa nella Conv 1×1
	g = g.bytes(1, nodePB("Add", []string{"p1", "q"}, []string{"a0"}))
	g = g.bytes(1, nodePB("PRelu", []string{"a0", "s3"}, []string{"a"})) // fusa nella Add
	g = g.bytes(1, nodePB("MaxPool", []string{"a"}, []string{"mp"}, attrIntsPB("kernel_shape", 2, 2), attrIntsPB("strides", 2, 2)))
	g = g.bytes(1, nodePB("Pad", []string{"mp", "pads"}, []string{"pd"}))
	g = g.bytes(1, nodePB("Conv", []string{"pd", "wh", "bh"}, []string{"hd"}, attrIntsPB("kernel_shape", 2, 2)))
	g = g.bytes(1, nodePB("Sigmoid", []string{"hd"}, []string{"sg"}))
	g = g.bytes(1, nodePB("Reshape", []string{"sg", "shape"}, []string{"out"}))
	g = g.bytes(5, floatTensorPB("w1", []int{4, 3, 3, 3}, w1)).bytes(5, floatTensorPB("b1", []int{4}, b1))
	g = g.bytes(5, floatTensorPB("s1", []int{1, 4, 1, 1}, s1))
	g = g.bytes(5, floatTensorPB("s2", []int{1, 4, 1, 1}, s2)).bytes(5, floatTensorPB("s3", []int{1}, s3))
	g = g.bytes(5, floatTensorPB("wd", []int{4, 1, 3, 3}, wd)).bytes(5, floatTensorPB("bd", []int{4}, bd))
	g = g.bytes(5, floatTensorPB("wp", []int{4, 4, 1, 1}, wp)).bytes(5, floatTensorPB("bp", []int{4}, bp))
	g = g.bytes(5, floatTensorPB("wh", []int{5, 8, 2, 2}, wh)).bytes(5, floatTensorPB("bh", []int{5}, bh))
	g = g.bytes(5, intTensorPB("pads", 0, 0, 0, 0, 0, 4, 0, 0))
	g = g.bytes(5, intTensorPB("shape", -1, 5))
	g = g.bytes(11, pb(nil).str(1, "in")).bytes(12, pb(nil).str(1, "out"))
	model := pb(nil).int(1, 8).bytes(7, g)

	m, err := Parse(model, []int{1, H, W, 3})
	if err != nil {
		t.Fatal(err)
	}
	in := randn(r, H*W*3)
	copy(m.Input(), in)
	simd := hasAVX2FMA
	defer func() { hasAVX2FMA = simd }()
	for _, cfg := range []struct {
		workers int
		simd    bool
	}{{1, false}, {3, false}, {1, simd}, {3, simd}} {
		workers := cfg.workers
		hasAVX2FMA = cfg.simd
		m.SetWorkers(workers)
		m.Run()
		got, err := m.Output("out")
		if err != nil {
			t.Fatal(err)
		}

		// riferimento
		x := ref{3, H, W, make([]float64, 3*H*W)}
		for y := 0; y < H; y++ {
			for xx := 0; xx < W; xx++ {
				for c := 0; c < 3; c++ {
					x.d[(c*H+y)*W+xx] = float64(in[(y*W+xx)*3+c])
				}
			}
		}
		c1 := refConv(x, w1, 3, 4, 3, b1, 2, 0, 0, 1, 1, 1)
		p1 := refPRelu(c1, s1)
		dw := refConv(p1, wd, 1, 4, 3, bd, 1, 1, 1, 1, 1, 4)
		pw := refConv(dw, wp, 4, 4, 1, bp, 1, 0, 0, 0, 0, 1)
		q := refPRelu(pw, s2)
		a := ref{4, p1.h, p1.w, make([]float64, len(p1.d))}
		for i := range a.d {
			a.d[i] = p1.d[i] + q.d[i]
			if a.d[i] < 0 {
				a.d[i] *= float64(s3[0])
			}
		}
		mp := ref{8, a.h / 2, a.w / 2, make([]float64, 8*(a.h/2)*(a.w/2))}
		for c := 0; c < 4; c++ {
			for y := 0; y < mp.h; y++ {
				for xx := 0; xx < mp.w; xx++ {
					best := math.Inf(-1)
					for ky := 0; ky < 2; ky++ {
						for kx := 0; kx < 2; kx++ {
							best = math.Max(best, a.at(c, 2*y+ky, 2*xx+kx))
						}
					}
					mp.d[(c*mp.h+y)*mp.w+xx] = best
				}
			}
		}
		hd := refConv(mp, wh, 8, 5, 2, bh, 1, 0, 0, 0, 0, 1)
		if len(got) != len(hd.d) {
			t.Fatalf("output %d values, reference %d", len(got), len(hd.d))
		}
		for i, v := range hd.d {
			want := 1 / (1 + math.Exp(-v))
			if math.Abs(float64(got[i])-want) > 1e-5 {
				t.Fatalf("workers=%d simd=%v out[%d] = %v, want %v", workers, hasAVX2FMA, i, got[i], want)
			}
		}
	}
	if m.MACs() == 0 {
		t.Fatal("no MACs counted")
	}
	for _, st := range m.steps {
		if st.op == "PRelu" && st.in[0].name != "c1" {
			t.Errorf("PRelu on %q not fused", st.in[0].name)
		}
	}
	if len(m.steps) != 10 {
		t.Errorf("%d steps after fusion, want 10", len(m.steps))
	}
	// a regime Run non alloca, né con una né con più goroutine
	for _, w := range []int{1, 3} {
		m.SetWorkers(w)
		m.Run()
		if n := testing.AllocsPerRun(20, m.Run); n != 0 {
			t.Errorf("workers=%d: %.1f allocations per Run", w, n)
		}
	}
	m.Close()
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, data := range [][]byte{nil, {0x3a, 0xff, 0xff, 0xff, 0xff, 0x0f}, {0x08}} {
		if _, err := Parse(data, []int{1, 4, 4, 3}); err == nil {
			t.Errorf("accepted %x", data)
		}
	}
	g := pb(nil).bytes(1, nodePB("Gemm", []string{"in"}, []string{"out"})).
		bytes(11, pb(nil).str(1, "in")).bytes(12, pb(nil).str(1, "out"))
	if _, err := Parse(pb(nil).bytes(7, g), []int{1, 4}); err == nil {
		t.Error("accepted an unsupported operator")
	}
}

func TestFaceModelLoads(t *testing.T) {
	m, err := Parse(facemodel.ONNX, []int{1, 256, 256, 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Identity", "Identity_1"} {
		if _, err := m.Output(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("MACs %.1fM, buffers %.1f MB", float64(m.MACs())/1e6, m.BufferMB())
}

func BenchmarkFaceModel(b *testing.B) {
	m, err := Parse(facemodel.ONNX, []int{1, 256, 256, 3})
	if err != nil {
		b.Fatal(err)
	}
	for i := range m.Input() {
		m.Input()[i] = 0.5
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Run()
	}
}
