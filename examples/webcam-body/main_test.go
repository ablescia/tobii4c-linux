//go:build linux

package main

import (
	"errors"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/bodypose"
	"github.com/ablescia/tobii4c-linux/internal/tflite"
	"github.com/ablescia/tobii4c-linux/internal/webcam"
)

// testEngine è un motore senza webcam né modelli: l'interfaccia legge solo
// lo stato pubblicato.
func testEngine(t *testing.T) *Engine {
	in := webcam.CamInfo{Path: "/dev/video-prova", Card: "Webcam di prova", W: 640, H: 360, PixFmt: webcam.PixMJPG, FPS: 30}
	cpu := tflite.NewCPU(1)
	t.Cleanup(cpu.Close)
	e := &Engine{Info: in, CPU: cpu, GPUErr: "assente nel test", conv: webcam.NewConverter(&in, 1), Focal: 500, FOV: 65,
		infos: map[Config]ModelInfo{}, trackers: map[Config]*bodypose.Tracker{}}
	e.want = Config{Mode: bodypose.FullBody, Variant: bodypose.Full}
	e.cur = e.want
	return e
}

// publish pubblica un frame a colori con una persona in piedi.
func publish(e *Engine) {
	f := webcam.NewFrame(640, 360, true)
	f.CW, f.CH = 320, 360
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			f.Y[y*640+x] = byte(40 + x/4)
		}
		for x := 0; x < 320; x++ {
			f.Cb[y*320+x], f.Cr[y*320+x] = byte(90+y/3), byte(160-x/4)
		}
	}
	p := bodypose.Pose{Mode: bodypose.FullBody, Found: true, Score: 0.99, N: bodypose.NumLandmarks, HasWorld: true,
		ROI: bodypose.ROI{CX: 320, CY: 200, W: 300, H: 300}, MaskW: 16, MaskH: 16}
	world := map[int][3]float64{
		bodypose.Nose: {0, -0.7, -0.1}, bodypose.LeftShoulder: {0.2, -0.5, 0}, bodypose.RightShoulder: {-0.2, -0.5, 0},
		bodypose.LeftElbow: {0.25, -0.2, 0}, bodypose.RightElbow: {-0.25, -0.2, 0}, bodypose.LeftWrist: {0.25, 0.05, 0},
		bodypose.RightWrist: {-0.3, -0.75, 0}, bodypose.LeftHip: {0.1, 0, 0}, bodypose.RightHip: {-0.1, 0, 0},
		bodypose.LeftKnee: {0.1, 0.45, 0}, bodypose.RightKnee: {-0.1, 0.45, 0}, bodypose.LeftAnkle: {0.1, 0.9, 0},
		bodypose.RightAnkle: {-0.1, 0.9, 0},
	}
	for i, w := range world {
		p.World[i] = w
		p.LM[i] = bodypose.Landmark{X: 320 + 150*w[0], Y: 220 + 150*w[1], Visibility: 1, Presence: 1}
	}
	for i := range p.LM {
		if _, ok := world[i]; !ok {
			p.LM[i] = bodypose.Landmark{X: 320, Y: 120, Visibility: 0.2, Presence: 0.9}
		}
	}
	p.Mask = make([]float32, 16*16)
	for i := range p.Mask {
		if x, y := i%16, i/16; x > 4 && x < 11 && y > 2 {
			p.Mask[i] = 1
		}
	}
	m := bodypose.Measure(&p, e.Focal)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.res = Result{Seq: 1, Cfg: e.cur, Pose: p, M: m, LmMs: 1.5, TotalMs: 2, InfMean: 2, InfP95: 2.2, InfMax: 3}
	e.res.frame.CopyFrom(f)
	e.have, e.rawSeq = true, 1
	now := time.Now()
	for i := 0; i < 50; i++ {
		hp := HistPoint{T: now.Add(time.Duration(i-50) * 100 * time.Millisecond), Found: true, TotalMs: 2 + 0.1*math.Sin(float64(i)),
			Lean: float64(i % 7), Tilt: -2, Forward: nan}
		for j := range hp.Joints {
			hp.Joints[j] = 90 + float64(i)
		}
		e.hist[e.histN%histLen] = hp
		e.histN++
	}
}

func TestRender(t *testing.T) {
	e := testEngine(t)
	publish(e)
	a := NewApp(nil, e, 1920, 1080, true)
	if err := a.resize(1920, 1080); err != nil {
		t.Fatal(err)
	}
	a.paint(time.Now())
	a.full = true
	a.paint(time.Now())
	if !a.snap.Overlay || !a.snap.Res.Pose.Found || a.snap.Res.M.Posture != bodypose.PostureStanding {
		t.Fatalf("stato non letto: overlay %v, postura %d", a.snap.Overlay, a.snap.Res.M.Posture)
	}
	// l'anteprima è a colori: il pixel al centro non è grigio
	r := a.previewRect(a.cards[1].body)
	c := a.canvas.RGBAAt(r.Min.X+r.Dx()/4, r.Min.Y+r.Dy()/8)
	if c.R == c.G && c.G == c.B {
		t.Errorf("anteprima in bianco e nero: %v", c)
	}
	if dir := os.Getenv("TOBII4C_BODY_PNG"); dir != "" {
		for _, mode := range []int{drawSkeleton, drawSkeletonMask} {
			a.drawMode = mode
			a.full = true
			a.paint(time.Now())
			p := filepath.Join(dir, "webcam-body-"+drawNames[mode]+".png")
			if err := savePNG(p, a.canvas); err != nil {
				t.Fatal(err)
			}
			t.Log("salvata", p)
		}
	}
}

func TestRadioClick(t *testing.T) {
	e := testEngine(t)
	a := NewApp(nil, e, 1600, 900, true)
	if err := a.resize(1600, 900); err != nil {
		t.Fatal(err)
	}
	a.paint(time.Now())
	center := func(g, i int) image.Point {
		r := a.radios[g].rects[i]
		return image.Pt((r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2)
	}
	click := func(p image.Point) {
		for _, g := range a.radios {
			g.click(a, p)
		}
		a.paint(time.Now())
	}
	// "Mezzo busto"
	click(center(0, 1))
	if w := e.Wanted(); w.Mode != bodypose.UpperBody || w.Variant != bodypose.Full {
		t.Fatalf("dopo il clic su mezzo busto: %+v", w)
	}
	// le varianti sono disabilitate nel mezzo busto
	click(center(1, 2))
	if e.Wanted().Variant != bodypose.Full {
		t.Fatal("variante cambiata con il mezzo busto")
	}
	// di nuovo corpo intero, poi heavy
	click(center(0, 0))
	click(center(1, 2))
	if w := e.Wanted(); w.Mode != bodypose.FullBody || w.Variant != bodypose.Heavy {
		t.Fatalf("dopo corpo intero e heavy: %+v", w)
	}
	// senza GPU la voce è disabilitata e la CPU resta scelta
	click(center(2, 0))
	if e.Wanted().GPU {
		t.Fatal("GPU scelta senza GPU")
	}
	// passaggio del mouse sopra una voce attiva
	if !a.radios[0].hover(a, center(0, 1)) || a.radios[0].hot != 1 {
		t.Error("evidenziazione sotto il mouse non aggiornata")
	}
	// tasti
	a.key('b')
	if e.Wanted().Mode != bodypose.UpperBody {
		t.Error("tasto B")
	}
	a.key('b')
	a.key('1')
	if e.Wanted().Variant != bodypose.Lite {
		t.Error("tasto 1")
	}
}

func TestThickLineClip(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	clip := image.Rect(20, 20, 80, 80)
	thickLine(img, clip, -50, 50, 150, 50, 6, colLeft, 1)
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			on := img.RGBAAt(x, y).A != 0
			if on && !image.Pt(x, y).In(clip) {
				t.Fatalf("pixel %d,%d fuori dal ritaglio", x, y)
			}
		}
	}
	if img.RGBAAt(50, 50) != colLeft || img.RGBAAt(50, 52) != colLeft || img.RGBAAt(50, 56).A != 0 {
		t.Errorf("spessore sbagliato: %v %v %v", img.RGBAAt(50, 50), img.RGBAAt(50, 52), img.RGBAAt(50, 56))
	}
}

// TestEngineSwitch: il motore elabora un frame vero, poi passa al mezzo
// busto e alla CPU tra un frame e l'altro (come dopo un clic sui radio
// button), caricando i modelli dalla cache.
func TestEngineSwitch(t *testing.T) {
	e := testEngine(t)
	if g, err := tflite.NewGPU(); err == nil {
		e.GPU, e.GPUErr = g, ""
		t.Cleanup(g.Close)
	}
	e.offline = true
	start := Config{Mode: bodypose.FullBody, Variant: bodypose.Lite, GPU: e.GPU != nil}
	if err := e.Prepare(start, nil); err != nil {
		if errors.Is(err, bodypose.ErrOffline) {
			t.Skip("modelli non scaricati")
		}
		t.Fatal(err)
	}
	f := testFrame(t)
	run := func(want Config) *Result {
		e.switchIfNeeded()
		e.handle(f)
		var s Snapshot
		e.Snapshot(&s)
		if s.LoadErr != "" {
			t.Fatal(s.LoadErr)
		}
		if s.Res.Cfg.key() != want.key() {
			t.Fatalf("elaborato con %v, atteso %v", s.Res.Cfg, want)
		}
		return &s.Res
	}
	r := run(start)
	if !r.Pose.Found || r.Pose.N != bodypose.NumLandmarks || !r.Pose.DetRan {
		t.Fatalf("corpo intero: trovata %v, %d punti, detector %v", r.Pose.Found, r.Pose.N, r.Pose.DetRan)
	}
	if r = run(start); !r.Pose.Tracking {
		t.Error("il secondo frame non insegue il primo")
	}
	upper := Config{Mode: bodypose.UpperBody, Variant: bodypose.Lite, GPU: false}
	e.Request(upper)
	r = run(upper)
	if !r.Pose.Found || r.Pose.N != bodypose.NumUpperLandmarks || r.Pose.HasWorld || !r.Pose.DetRan {
		t.Fatalf("mezzo busto: trovata %v, %d punti, 3D %v, detector %v", r.Pose.Found, r.Pose.N, r.Pose.HasWorld, r.Pose.DetRan)
	}
	if n := e.switches.Load(); n != 1 {
		t.Errorf("%d cambi, atteso 1", n)
	}
	// configurazione impossibile da caricare: resta quella che funziona
	e.Request(Config{Mode: bodypose.FullBody, Variant: "enorme"})
	e.switchIfNeeded()
	e.handle(f)
	var s Snapshot
	e.Snapshot(&s)
	if s.LoadErr == "" || s.Want.key() != upper.key() || s.Res.Cfg.key() != upper.key() {
		t.Errorf("errore di caricamento %q, configurazione %v, elaborato con %v", s.LoadErr, s.Want, s.Res.Cfg)
	}
}

// testFrame legge la foto di prova di internal/bodypose come frame YCbCr.
func testFrame(t *testing.T) *frame {
	fh, err := os.Open("../../internal/bodypose/testdata/console.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	im, err := jpeg.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	m := im.(*image.YCbCr)
	w, h := m.Rect.Dx(), m.Rect.Dy()
	f := &frame{Frame: *webcam.NewFrame(w, h, true), seq: 1, captured: time.Now()}
	f.CW, f.CH = (w+1)/2, (h+1)/2
	for y := 0; y < h; y++ {
		copy(f.Y[y*w:(y+1)*w], m.Y[y*m.YStride:])
	}
	for y := 0; y < f.CH; y++ {
		copy(f.Cb[y*f.CW:(y+1)*f.CW], m.Cb[y*m.CStride:])
		copy(f.Cr[y*f.CW:(y+1)*f.CW], m.Cr[y*m.CStride:])
	}
	return f
}
