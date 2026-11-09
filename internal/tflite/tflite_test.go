package tflite

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// testInput è l'ingresso deterministico dei riferimenti di LiteRT
// (testdata/litert_reference.json, generato con lo stesso calcolo in numpy).
func testInput(h, w int, pm1 bool) []float32 {
	out := make([]float32, h*w*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < 3; c++ {
				fx, fy, fc := float64(x), float64(y), float64(c)
				v := 0.5 + 0.5*math.Sin(0.07*fx*(fc+1)+0.05*fy)*math.Cos(0.03*fy*(fc+1)+0.02*fx)
				f := float32(v)
				if pm1 {
					f = f*2 - 1
				}
				out[(y*w+x)*3+c] = f
			}
		}
	}
	return out
}

func modelsDir() string {
	if d := os.Getenv("TOBII4C_MODELS"); d != "" {
		return d
	}
	d, _ := os.UserCacheDir()
	return filepath.Join(d, "tobii4c", "models")
}

// loadCached legge un modello dalla cache dei modelli (eventualmente da un
// archivio .task); il test viene saltato se manca.
func loadCached(t *testing.T, file, member string) *Model {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(modelsDir(), file))
	if os.IsNotExist(err) {
		t.Skipf("%s non è nella cache dei modelli (avvia una volta examples/webcam-body)", file)
	}
	if err != nil {
		t.Fatal(err)
	}
	if member != "" {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		f, err := zr.Open(member)
		if err != nil {
			t.Fatal(err)
		}
		data, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func testBackends(t *testing.T) []Backend {
	bs := []Backend{NewCPU(0)}
	if g, err := NewGPU(); err == nil {
		bs = append(bs, g)
	} else {
		t.Logf("GPU non disponibile, solo CPU: %v", err)
	}
	t.Cleanup(func() {
		for _, b := range bs {
			b.Close()
		}
	})
	return bs
}

// TestLiteRTReference esegue le reti di MediaPipe Pose e confronta le uscite
// con quelle di LiteRT (ai-edge-litert, XNNPACK) sullo stesso ingresso.
func TestLiteRTReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/litert_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Models []struct {
			File, Member string
			PM1          bool
			Outputs      []struct {
				Name      string
				N, Stride int
				Scale     float64
				Values    []float64
			}
		}
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	backends := testBackends(t)
	for _, rm := range ref.Models {
		m := loadCached(t, rm.File, rm.Member)
		g, err := Compile(m)
		if err != nil {
			t.Fatalf("%s: %v", rm.File, err)
		}
		is := g.InputShape()
		in := testInput(is[1], is[2], rm.PM1)
		for _, b := range backends {
			r, err := b.Load(g)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.RunTensor(in); err != nil {
				t.Fatal(err)
			}
			for k, o := range rm.Outputs {
				got := r.Output(k)
				if len(got) != o.N || g.Names[k] != o.Name {
					t.Fatalf("%s %s: uscita %d %q di %d valori, attesa %q di %d", rm.File, rm.Member, k, g.Names[k], len(got), o.Name, o.N)
				}
				worst := 0.0
				for i, want := range o.Values {
					worst = math.Max(worst, math.Abs(float64(got[i*o.Stride])-want))
				}
				rel := worst / math.Max(o.Scale, 1e-6)
				t.Logf("%s %s %s %s: scarto massimo %.3g (relativo %.2g)", b.Name(), rm.File, rm.Member, o.Name, worst, rel)
				// la segmentazione ha logit fino a ~10³: l'ordine delle somme pesa di più
				if rel > 5e-3 {
					t.Errorf("%s %s %s %s: scarto relativo %.3g", b.Name(), rm.File, rm.Member, o.Name, rel)
				}
			}
			r.Close()
		}
	}
}

func TestHalf(t *testing.T) {
	for _, c := range []struct {
		h    uint16
		want float32
	}{
		{0x3c00, 1}, {0xc000, -2}, {0x7bff, 65504}, {0x3555, 0.33325195}, {0x0001, 5.9604645e-08},
		{0x0400, 6.1035156e-05}, {0x8000, float32(math.Copysign(0, -1))},
	} {
		if got := half(c.h); got != c.want || math.Signbit(float64(got)) != math.Signbit(float64(c.want)) {
			t.Errorf("half(%#04x) = %g, atteso %g", c.h, got, c.want)
		}
	}
	if !math.IsInf(float64(half(0x7c00)), 1) || !math.IsNaN(float64(half(0x7e00))) {
		t.Error("infiniti o NaN sbagliati")
	}
}

// synthetic costruisce un modello con tutti gli operatori gestiti e le
// fusioni del piano: PAD → conv 3×3 a passo 2 (VALID), depthwise SAME,
// conv 1×1 + ADD residua, conv 1×1 su una mappa grande (tile) e piccola
// (righe), MAX_POOL, RESIZE_BILINEAR, DEPTH_TO_SPACE, CONCATENATION,
// MUL per canale, LOGISTIC, conv che copre tutto l'ingresso, RESHAPE.
func synthetic() *Model {
	m := &Model{}
	seed := uint32(7)
	rnd := func() float32 {
		seed = seed*1664525 + 1013904223
		return float32(seed>>8)/float32(1<<24) - 0.5
	}
	tensor := func(shape ...int) int {
		m.Tensors = append(m.Tensors, Tensor{Shape: shape, Type: TypeFloat32})
		return len(m.Tensors) - 1
	}
	konst := func(shape ...int) int {
		i := tensor(shape...)
		t := &m.Tensors[i]
		t.Const, t.Data = true, make([]float32, elems(shape))
		for k := range t.Data {
			t.Data[k] = rnd() * 0.5
		}
		return i
	}
	ints := func(v ...int32) int {
		i := tensor(len(v))
		m.Tensors[i].Type, m.Tensors[i].Const, m.Tensors[i].Ints = TypeInt32, true, v
		return i
	}
	op := func(code int, opt Options, out int, in ...int) int {
		m.Ops = append(m.Ops, Op{Code: code, Inputs: in, Outputs: []int{out}, Opt: opt})
		return out
	}
	one := Options{StrideW: 1, StrideH: 1, DilationW: 1, DilationH: 1, DepthMult: 1}
	x := tensor(1, 33, 47, 3)
	m.Inputs = []int{x}
	p := op(OpPad, one, tensor(1, 35, 49, 3), x, ints(0, 0, 1, 1, 1, 1, 0, 0))
	o := one
	o.Padding, o.StrideW, o.StrideH, o.Act = PadValid, 2, 2, ActRelu6
	c1 := op(OpConv2D, o, tensor(1, 17, 24, 16), p, konst(16, 3, 3, 3), konst(16))
	o = one
	o.Padding, o.Act = PadSame, ActRelu
	d1 := op(OpDepthwiseConv, o, tensor(1, 17, 24, 16), c1, konst(1, 3, 3, 16), konst(16))
	c2 := op(OpConv2D, one, tensor(1, 17, 24, 16), d1, konst(16, 1, 1, 16), konst(16))
	a1 := op(OpAdd, Options{Act: ActRelu6}, tensor(1, 17, 24, 16), c2, c1)
	big := op(OpConv2D, one, tensor(1, 17, 24, 200), a1, konst(200, 1, 1, 16), konst(200))
	o = one
	o.Padding, o.StrideW, o.StrideH, o.FilterW, o.FilterH = PadValid, 2, 2, 2, 2
	mp := op(OpMaxPool2D, o, tensor(1, 8, 12, 200), big)
	small := op(OpConv2D, one, tensor(1, 8, 12, 24), mp, konst(24, 1, 1, 200), konst(24))
	rs := op(OpResizeBilinear, Options{HalfPixel: true}, tensor(1, 16, 24, 24), small, ints(16, 24))
	ds := op(OpDepthToSpace, Options{Block: 2}, tensor(1, 32, 48, 6), rs)
	o = one
	o.Padding, o.StrideW, o.StrideH = PadSame, 2, 2
	dsd := op(OpDepthwiseConv, o, tensor(1, 16, 24, 6), ds, konst(1, 3, 3, 6), konst(6))
	cat := op(OpConcatenation, Options{Axis: -1}, tensor(1, 16, 24, 30), rs, dsd)
	mul := op(OpMul, Options{}, tensor(1, 16, 24, 30), cat, konst(30))
	sg := op(OpLogistic, one, tensor(1, 16, 24, 30), mul)
	o = one
	o.Padding, o.StrideW, o.StrideH, o.FilterW, o.FilterH = PadValid, 4, 4, 4, 4
	ap := op(OpAveragePool2D, o, tensor(1, 4, 6, 30), sg)
	o = one
	o.Padding = PadValid
	fc := op(OpConv2D, o, tensor(1, 1, 1, 10), ap, konst(10, 4, 6, 30), konst(10))
	out := op(OpReshape, Options{}, tensor(1, 10), fc, ints(1, 10))
	m.Outputs = []int{out, cat}
	return m
}

func TestSyntheticGraph(t *testing.T) {
	g, err := Compile(synthetic())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[Kind]int{}
	for _, n := range g.Nodes {
		kinds[n.Kind]++
		if n.Kind == KPad {
			t.Error("PAD non fuso nella convoluzione")
		}
	}
	if kinds[KBinary] != 1 || kinds[KPointwise] != 4 || kinds[KConv] != 1 {
		t.Errorf("fusioni non avvenute: %v", kinds)
	}
	t.Log(g.Stats())
	in := testInput(33, 47, true)
	var outs [][]float32
	for _, b := range testBackends(t) {
		r, err := b.Load(g)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.RunTensor(in); err != nil {
			t.Fatal(err)
		}
		cp := append([]float32(nil), r.Output(0)...)
		cp = append(cp, r.Output(1)...)
		outs = append(outs, cp)
		r.Close()
	}
	for i, v := range outs[0] {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("uscita %d non finita", i)
		}
	}
	if len(outs) == 2 {
		worst := 0.0
		for i := range outs[0] {
			worst = math.Max(worst, math.Abs(float64(outs[0][i]-outs[1][i])))
		}
		t.Logf("GPU − CPU: %.3g", worst)
		if worst > 1e-4 {
			t.Errorf("GPU e CPU diverse di %.3g", worst)
		}
	}
}

// TestWarp confronta il warp della GPU con quello della CPU su
// un'immagine YCbCr 4:2:2 ruotata, con bordo nero e replicato.
func TestWarp(t *testing.T) {
	g, err := Compile(synthetic())
	if err != nil {
		t.Fatal(err)
	}
	const w, h = 64, 40
	im := &Image{W: w, H: h, Y: make([]byte, w*h), CW: w / 2, CH: h, Cb: make([]byte, w/2*h), Cr: make([]byte, w/2*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Y[y*w+x] = byte(x*4 + y)
		}
		for x := 0; x < w/2; x++ {
			im.Cb[y*w/2+x], im.Cr[y*w/2+x] = byte(100+x*3), byte(200-y*2)
		}
	}
	bs := testBackends(t)
	for _, rep := range []bool{false, true} {
		s, c := math.Sincos(0.4)
		wp := Warp{M: [6]float32{float32(c) * 1.3, float32(-s) * 1.3, 10, float32(s) * 1.3, float32(c) * 1.3, -8}, Lo: -1, Hi: 1, Replicate: rep}
		var ins [][]float32
		for _, b := range bs {
			r, err := b.Load(g)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.SetImage(im); err != nil {
				t.Fatal(err)
			}
			if err := r.RunImage(&wp); err != nil {
				t.Fatal(err)
			}
			in, err := r.Input()
			if err != nil {
				t.Fatal(err)
			}
			ins = append(ins, in)
			r.Close()
		}
		cpu := ins[0]
		if rep {
			for i, v := range cpu {
				if v < -1-1e-5 || v > 1+1e-5 {
					t.Fatalf("valore %d fuori da [-1, 1]: %g", i, v)
				}
			}
		}
		if len(ins) == 2 {
			worst := 0.0
			for i := range cpu {
				worst = math.Max(worst, math.Abs(float64(cpu[i]-ins[1][i])))
			}
			t.Logf("bordo replicato %v: GPU − CPU %.3g", rep, worst)
			if worst > 1e-3 {
				t.Errorf("warp diverso tra GPU e CPU di %.3g", worst)
			}
		}
	}
	// identità su un'immagine grigia: il tensore riproduce i pixel
	gray := &Image{W: 47, H: 33, Y: make([]byte, 47*33)}
	for i := range gray.Y {
		gray.Y[i] = byte(i * 7)
	}
	cpu := NewCPU(1)
	defer cpu.Close()
	r, _ := cpu.Load(g)
	cpu.SetImage(gray)
	if err := r.RunImage(&Warp{M: [6]float32{1, 0, 0, 0, 1, 0}, Lo: 0, Hi: 255}); err != nil {
		t.Fatal(err)
	}
	in, _ := r.Input()
	for i, v := range gray.Y {
		for c := 0; c < 3; c++ {
			if in[3*i+c] != float32(v) {
				t.Fatalf("pixel %d canale %d: %g invece di %d", i, c, in[3*i+c], v)
			}
		}
	}
}

// BenchmarkModels: tempo per inferenza delle reti di MediaPipe Pose (dalla
// cache dei modelli) sulla CPU e sulla GPU; per la GPU anche il tempo dai
// timestamp di Vulkan (gpu-ms/op).
//
//	go test -run '^$' -bench Models ./internal/tflite
func BenchmarkModels(b *testing.B) {
	t := &testing.T{}
	models := []struct{ name, file, member string }{
		{"detector-full", "pose_landmarker_full.task", "pose_detector.tflite"},
		{"landmark-lite", "pose_landmarker_lite.task", "pose_landmarks_detector.tflite"},
		{"landmark-full", "pose_landmarker_full.task", "pose_landmarks_detector.tflite"},
		{"landmark-heavy", "pose_landmarker_heavy.task", "pose_landmarks_detector.tflite"},
		{"detector-upper", "mediapipe-0.8.3.2-pose_detection.tflite", ""},
		{"landmark-upper", "mediapipe-0.8.3.2-pose_landmark_upper_body.tflite", ""},
	}
	backends := []Backend{NewCPU(0)}
	if g, err := NewGPU(); err == nil {
		backends = append(backends, g)
	}
	defer func() {
		for _, be := range backends {
			be.Close()
		}
	}()
	for _, mm := range models {
		if _, err := os.Stat(filepath.Join(modelsDir(), mm.file)); err != nil {
			b.Skipf("%s non è nella cache dei modelli", mm.file)
		}
		m := loadCached(t, mm.file, mm.member)
		g, err := Compile(m)
		if err != nil {
			b.Fatal(err)
		}
		is := g.InputShape()
		in := testInput(is[1], is[2], false)
		for _, be := range backends {
			r, err := be.Load(g)
			if err != nil {
				b.Fatal(err)
			}
			b.Run(mm.name+"/"+be.Name(), func(b *testing.B) {
				var gpu float64
				for i := 0; i < b.N; i++ {
					if err := r.RunTensor(in); err != nil {
						b.Fatal(err)
					}
					gpu += float64(r.Timing().GPU.Nanoseconds())
				}
				if gpu > 0 {
					b.ReportMetric(gpu/float64(b.N)/1e6, "gpu-ms/op")
				}
				b.ReportMetric(float64(g.MACs)/1e6, "MMAC")
			})
			r.Close()
		}
	}
}
