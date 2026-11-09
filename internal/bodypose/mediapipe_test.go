package bodypose

import (
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"os"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/tflite"
)

// Le immagini di testdata sono foto NASA di pubblico dominio (vedi
// testdata/README.md); mediapipe_reference.json contiene i punti che
// MediaPipe 0.10.14 (Pose Landmarker, modalità IMAGE) dà sulle stesse
// immagini convertite in RGB come fa il warp di internal/tflite.

type refPose struct {
	LM    [][5]float64 `json:"lm"`
	World [][3]float64 `json:"world"`
}

type refFile struct {
	MediaPipe string `json:"mediapipe"`
	Images    map[string]struct {
		W, H     int
		Variants map[string]*refPose `json:"variants"`
	} `json:"images"`
}

// testImage legge un JPEG di testdata nei piani YCbCr.
func testImage(t testing.TB, name string) *tflite.Image {
	t.Helper()
	fh, err := os.Open("testdata/" + name + ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	im, err := jpeg.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := im.(*image.YCbCr)
	if !ok || m.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Fatalf("%s: atteso un JPEG 4:2:0", name)
	}
	w, h := m.Rect.Dx(), m.Rect.Dy()
	cw, ch := (w+1)/2, (h+1)/2
	img := &tflite.Image{W: w, H: h, Y: make([]byte, w*h), CW: cw, CH: ch, Cb: make([]byte, cw*ch), Cr: make([]byte, cw*ch)}
	for y := 0; y < h; y++ {
		copy(img.Y[y*w:(y+1)*w], m.Y[y*m.YStride:])
	}
	for y := 0; y < ch; y++ {
		copy(img.Cb[y*cw:(y+1)*cw], m.Cb[y*m.CStride:])
		copy(img.Cr[y*cw:(y+1)*cw], m.Cr[y*m.CStride:])
	}
	return img
}

// cachedModels legge i modelli dalla cache (senza scaricarli): il test
// viene saltato se mancano.
func cachedModels(t testing.TB, m Mode, variant string) *Models {
	t.Helper()
	dir := os.Getenv("TOBII4C_MODELS")
	ms, err := Load(m, variant, dir, true, nil)
	if errors.Is(err, ErrOffline) {
		t.Skipf("modelli non scaricati (avvia una volta examples/webcam-body o imposta TOBII4C_MODELS): %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func testBackends(t testing.TB) []tflite.Backend {
	bs := []tflite.Backend{tflite.NewCPU(0)}
	if g, err := tflite.NewGPU(); err == nil {
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

// TestAgainstMediaPipe confronta il primo frame della pipeline (detector,
// ritaglio, landmark, raffinamento sulla heatmap, coordinate 3D) con
// MediaPipe, sulla CPU e sulla GPU.
//
// Le reti coincidono con LiteRT (vedi internal/tflite), ma l'ingresso no:
// MediaPipe campiona il ritaglio con cv::warpPerspective sull'immagine a 8
// bit (posizioni a 1/32 di pixel, valori arrotondati a interi), qui in
// float. Detector e landmark reagiscono a queste differenze minime di
// qualche pixel (emulare l'arrotondamento di OpenCV sposta gli scarti senza
// ridurli): le tolleranze sono quindi di pochi pixel su immagini di
// 512–640 pixel, abbastanza strette da cogliere un ritaglio, una rotazione o
// una decodifica sbagliati (decine o centinaia di pixel).
func TestAgainstMediaPipe(t *testing.T) {
	data, err := os.ReadFile("testdata/mediapipe_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref refFile
	if err := json.Unmarshal(data, &ref); err != nil {
		t.Fatal(err)
	}
	backends := testBackends(t)
	for _, variant := range Variants {
		ms := cachedModels(t, FullBody, variant)
		for _, b := range backends {
			tr, err := NewTracker(b, ms)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"portrait", "console"} {
				want := ref.Images[name].Variants[variant]
				tr.Reset()
				p, err := tr.Process(testImage(t, name), time.Now())
				if err != nil {
					t.Fatal(err)
				}
				tag := name + " " + variant + " " + b.Name()
				if want == nil {
					if p.Found {
						t.Errorf("%s: persona trovata, MediaPipe non la trova", tag)
					}
					continue
				}
				if !p.Found {
					t.Fatalf("%s: persona non trovata (presenza %.3f)", tag, p.Score)
				}
				var sum, worst, wsum, wworst, vis float64
				n := 0
				for i := 0; i < NumLandmarks; i++ {
					r := want.LM[i]
					vis = math.Max(vis, math.Abs(p.Raw[i].Visibility-r[3]))
					if r[3] < 0.5 || r[4] < 0.5 {
						continue
					}
					d := math.Hypot(p.Raw[i].X-r[0], p.Raw[i].Y-r[1])
					sum += d
					worst = math.Max(worst, d)
					wd := math.Sqrt(math.Pow(p.World[i][0]-want.World[i][0], 2) + math.Pow(p.World[i][1]-want.World[i][1], 2) +
						math.Pow(p.World[i][2]-want.World[i][2], 2))
					wsum += wd
					wworst = math.Max(wworst, wd)
					n++
				}
				t.Logf("%s: %d punti visibili · scarto medio %.2f px, massimo %.2f px · 3D medio %.1f mm, massimo %.1f mm · visibilità ±%.3f",
					tag, n, sum/float64(n), worst, 1000*wsum/float64(n), 1000*wworst, vis)
				if n < 10 {
					t.Errorf("%s: solo %d punti visibili nel riferimento", tag, n)
				}
				if sum/float64(n) > 7 || worst > 25 {
					t.Errorf("%s: punti lontani da MediaPipe (medio %.2f px, massimo %.2f px)", tag, sum/float64(n), worst)
				}
				if 1000*wsum/float64(n) > 80 || 1000*wworst > 120 {
					t.Errorf("%s: coordinate 3D lontane da MediaPipe (medio %.1f mm, massimo %.1f mm)", tag, 1000*wsum/float64(n), 1000*wworst)
				}
				if vis > 0.35 {
					t.Errorf("%s: visibilità diversa da MediaPipe di %.3f", tag, vis)
				}
			}
			tr.Close()
		}
	}
}

// TestUpperBodyImages: il modello del mezzo busto trova la persona seduta e
// mette spalle e gomiti vicino a quelli del corpo intero di MediaPipe (il
// modello è un altro: tolleranza larga).
func TestUpperBodyImages(t *testing.T) {
	data, err := os.ReadFile("testdata/mediapipe_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref refFile
	if err := json.Unmarshal(data, &ref); err != nil {
		t.Fatal(err)
	}
	ms := cachedModels(t, UpperBody, "")
	for _, b := range testBackends(t) {
		tr, err := NewTracker(b, ms)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"portrait", "console"} {
			tr.Reset()
			img := testImage(t, name)
			p, err := tr.Process(img, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if !p.Found || p.N != NumUpperLandmarks || p.HasWorld {
				t.Fatalf("%s %s: trovata %v, %d punti, 3D %v", name, b.Name(), p.Found, p.N, p.HasWorld)
			}
			want := ref.Images[name].Variants[Heavy]
			diag := math.Hypot(float64(img.W), float64(img.H))
			for _, i := range []int{Nose, LeftShoulder, RightShoulder, LeftElbow, RightElbow} {
				if !p.Visible(i) {
					continue
				}
				d := math.Hypot(p.LM[i].X-want.LM[i][0], p.LM[i].Y-want.LM[i][1])
				t.Logf("%s %s: %s a %.1f px dal corpo intero di MediaPipe", name, b.Name(), Names[i], d)
				if d > 0.05*diag {
					t.Errorf("%s %s: %s a %.1f px dal riferimento", name, b.Name(), Names[i], d)
				}
			}
		}
		tr.Close()
	}
}

// TestGPUMatchesCPU: stessa immagine, stessi punti sulla GPU e sulla CPU
// (stesso piano, stessi pesi: solo l'ordine delle somme cambia).
func TestGPUMatchesCPU(t *testing.T) {
	gpu, err := tflite.NewGPU()
	if err != nil {
		t.Skipf("GPU non disponibile: %v", err)
	}
	defer gpu.Close()
	cpu := tflite.NewCPU(0)
	defer cpu.Close()
	for _, c := range []struct {
		mode    Mode
		variant string
	}{{FullBody, Lite}, {FullBody, Full}, {FullBody, Heavy}, {UpperBody, ""}} {
		ms := cachedModels(t, c.mode, c.variant)
		tg, err := NewTracker(gpu, ms)
		if err != nil {
			t.Fatal(err)
		}
		tc, err := NewTracker(cpu, ms)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"portrait", "console"} {
			img := testImage(t, name)
			now := time.Now()
			pg, err := tg.Process(img, now)
			if err != nil {
				t.Fatal(err)
			}
			g := *pg
			pc, err := tc.Process(img, now)
			if err != nil {
				t.Fatal(err)
			}
			tag := name + " " + c.mode.String() + " " + c.variant
			if g.Found != pc.Found || math.Abs(g.Score-pc.Score) > 1e-3 {
				t.Fatalf("%s: GPU trovata %v (%.4f), CPU %v (%.4f)", tag, g.Found, g.Score, pc.Found, pc.Score)
			}
			var worst, mask, maskSum float64
			for i := 0; i < g.N; i++ {
				worst = math.Max(worst, math.Hypot(g.Raw[i].X-pc.Raw[i].X, g.Raw[i].Y-pc.Raw[i].Y))
			}
			for i := range g.Mask {
				d := math.Abs(float64(g.Mask[i] - pc.Mask[i]))
				mask = math.Max(mask, d)
				maskSum += d
			}
			maskMean := maskSum / float64(max(len(g.Mask), 1))
			t.Logf("%s: GPU−CPU massimo %.4f px · maschera: media %.5f, massimo %.4f", tag, worst, maskMean, mask)
			// la maschera è una sigmoide di logit grandi: dove passano per
			// zero (il bordo della persona) piccole differenze contano di più
			if worst > 0.25 || maskMean > 0.002 || mask > 0.1 {
				t.Errorf("%s: GPU e CPU diverse (punti %.4f px, maschera %.5f / %.4f)", tag, worst, maskMean, mask)
			}
			tg.Reset()
			tc.Reset()
		}
		tg.Close()
		tc.Close()
	}
}
