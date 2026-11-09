package bodypose

import (
	"math"
	"testing"
	"time"
)

func TestAnchors(t *testing.T) {
	for _, c := range []struct {
		cfg   detectorConfig
		last  float64
		first float64
	}{{detectorFull, 6.5 / 7, 0.5 / 28}, {detectorUpper, 7.5 / 8, 0.5 / 16}} {
		a := ssdAnchors(c.cfg)
		if len(a) != c.cfg.anchors {
			t.Fatalf("%d ancore, attese %d", len(a), c.cfg.anchors)
		}
		if math.Abs(a[0].x-c.first) > 1e-12 || math.Abs(a[len(a)-1].y-c.last) > 1e-12 {
			t.Errorf("ancore: prima %+v, ultima %+v", a[0], a[len(a)-1])
		}
	}
}

func TestROI(t *testing.T) {
	r := ROI{CX: 300, CY: 200, W: 180, H: 180, Angle: 0.6}
	for _, uv := range [][2]float64{{0, 0}, {1, 0}, {0.3, 0.8}, {0.5, 0.5}, {1.2, -0.1}} {
		x, y := r.ToImage(uv[0], uv[1])
		u, v := r.FromImage(x, y)
		if math.Abs(u-uv[0]) > 1e-9 || math.Abs(v-uv[1]) > 1e-9 {
			t.Errorf("andata e ritorno %v → %v, %v", uv, u, v)
		}
	}
	// il warp porta il pixel (x, y) del tensore nel punto (x/S, y/S) del ritaglio
	const s = 256
	w := r.Warp(s, 0, 1, true)
	for _, p := range [][2]float64{{0, 0}, {255, 0}, {17, 200}} {
		gx := float64(w.M[0])*p[0] + float64(w.M[1])*p[1] + float64(w.M[2])
		gy := float64(w.M[3])*p[0] + float64(w.M[4])*p[1] + float64(w.M[5])
		ex, ey := r.ToImage(p[0]/s, p[1]/s)
		if math.Hypot(gx-ex, gy-ey) > 1e-3 {
			t.Errorf("warp di %v: %.3f %.3f invece di %.3f %.3f", p, gx, gy, ex, ey)
		}
	}
	// centro e punto di scala in verticale: nessuna rotazione
	up := roiFromPoints(100, 300, 100, 200, 1.25)
	if up.Angle != 0 || up.W != 250 || up.CX != 100 || up.CY != 300 {
		t.Errorf("ritaglio dritto: %+v", up)
	}
	// punto di scala a destra: il corpo è ruotato di 90° in senso orario
	right := roiFromPoints(100, 300, 200, 300, 1)
	if math.Abs(right.Angle-math.Pi/2) > 1e-12 || right.W != 200 {
		t.Errorf("ritaglio ruotato: %+v", right)
	}
	// il punto in alto al centro del ritaglio cade verso il punto di scala
	x, y := right.ToImage(0.5, 0.25)
	if math.Abs(x-150) > 1e-9 || math.Abs(y-300) > 1e-9 {
		t.Errorf("alto del ritaglio ruotato in %.1f %.1f", x, y)
	}
	lb := letterbox(1280, 720)
	if lb.W != 1280 || lb.CX != 640 || lb.CY != 360 {
		t.Errorf("letterbox %+v", lb)
	}
}

func TestWeightedNMS(t *testing.T) {
	d := func(s, x float64) Detection {
		return Detection{Score: s, Box: [4]float64{x, 0, x + 1, 1}, Keypoints: [4][2]float64{{x, 0}}}
	}
	out := weightedNMS([]Detection{d(0.6, 0.2), d(0.9, 0), d(0.7, 5)})
	if len(out) != 2 {
		t.Fatalf("%d riquadri, attesi 2", len(out))
	}
	// media pesata dei due sovrapposti (IoU 0,67), punteggio del migliore
	if want := 0.6 * 0.2 / 1.5; out[0].Score != 0.9 || math.Abs(out[0].Box[0]-want) > 1e-12 || math.Abs(out[0].Keypoints[0][0]-want) > 1e-12 {
		t.Errorf("primo gruppo %+v", out[0])
	}
	if out[1].Score != 0.7 || out[1].Box[0] != 5 {
		t.Errorf("secondo %+v", out[1])
	}
}

func TestOneEuro(t *testing.T) {
	f := oneEuro{minCutoff: 0.05, beta: 80, dCutoff: 1}
	t0 := time.Unix(0, 0)
	for i := 0; i < 30; i++ {
		if v := f.apply(t0.Add(time.Duration(i)*time.Second/30), 0.01, 42); v != 42 {
			t.Fatalf("valore costante filtrato in %v", v)
		}
	}
	// gradino: l'uscita parte verso il nuovo valore e ci arriva
	var v float64
	for i := 30; i < 90; i++ {
		v = f.apply(t0.Add(time.Duration(i)*time.Second/30), 0.01, 100)
		if i == 30 && (v <= 42 || v >= 100) {
			t.Fatalf("primo passo del gradino: %v", v)
		}
	}
	if math.Abs(v-100) > 1 {
		t.Errorf("dopo 2 s il filtro è a %v", v)
	}
	// timestamp non crescente: valore passato così com'è
	if got := f.apply(t0, 0.01, 7); got != 7 {
		t.Errorf("timestamp vecchio: %v", got)
	}
}

// pose costruisce una posa con le coordinate 3D date (metri, y in basso) e
// i punti dell'immagine come proiezione ortografica (1 m = 200 px).
func pose(world map[int][3]float64) *Pose {
	p := &Pose{Mode: FullBody, Found: true, N: NumLandmarks, HasWorld: true}
	for i, w := range world {
		p.World[i] = w
		p.LM[i] = Landmark{X: 320 + 200*w[0], Y: 240 + 200*w[1], Visibility: 1, Presence: 1}
	}
	return p
}

func TestMeasurePosture(t *testing.T) {
	// la sinistra del soggetto di fronte alla camera sta a x positiva
	standing := pose(map[int][3]float64{
		LeftShoulder: {0.2, -0.5, 0}, RightShoulder: {-0.2, -0.5, 0}, LeftHip: {0.1, 0, 0}, RightHip: {-0.1, 0, 0},
		LeftKnee: {0.1, 0.45, 0}, RightKnee: {-0.1, 0.45, 0}, LeftAnkle: {0.1, 0.9, 0}, RightAnkle: {-0.1, 0.9, 0},
		LeftElbow: {0.25, -0.2, 0}, RightElbow: {-0.25, -0.2, 0}, LeftWrist: {0.25, 0.05, 0}, RightWrist: {-0.25, -0.75, 0},
		LeftEye: {0.03, -0.75, -0.1}, RightEye: {-0.03, -0.75, -0.1},
	})
	m := Measure(standing, 1000)
	if m.Posture != PostureStanding {
		t.Errorf("in piedi riconosciuto come %q", PostureNames[m.Posture])
	}
	if j := m.Joints[JKneeL]; !j.OK || math.Abs(j.Deg-180) > 0.01 {
		t.Errorf("ginocchio sx dritto: %+v", j)
	}
	// braccio sx: spalla→gomito (0,05; 0,3), gomito→polso (0; 0,25)
	if want := 180 - math.Atan(0.05/0.3)*180/math.Pi; !m.Joints[JElbowL].OK || math.Abs(m.Joints[JElbowL].Deg-want) > 1e-9 {
		t.Errorf("gomito sx: %+v, atteso %.2f°", m.Joints[JElbowL], want)
	}
	if !m.HandUp[1] || m.HandUp[0] {
		t.Errorf("mani alzate %v, attesa solo la destra", m.HandUp)
	}
	if !m.LeanOK || math.Abs(m.LeanDeg) > 1e-9 || !m.ForwardOK || math.Abs(m.ForwardDeg) > 1e-9 || math.Abs(m.YawDeg) > 1e-9 {
		t.Errorf("busto dritto: lato %v avanti %v rotazione %v", m.LeanDeg, m.ForwardDeg, m.YawDeg)
	}
	// distanza: 1 m = 200 px con focale 1000 px → 5 m
	if !m.DistOK || math.Abs(m.DistM-5) > 1e-9 {
		t.Errorf("distanza %v", m.DistM)
	}

	sitting := pose(map[int][3]float64{
		LeftShoulder: {0.2, -0.5, 0.05}, RightShoulder: {-0.2, -0.5, 0.05}, LeftHip: {0.1, 0, 0}, RightHip: {-0.1, 0, 0},
		LeftKnee: {0.1, 0.02, -0.45}, RightKnee: {-0.1, 0.02, -0.45}, LeftAnkle: {0.1, 0.47, -0.45}, RightAnkle: {-0.1, 0.47, -0.45},
	})
	m = Measure(sitting, 1000)
	if m.Posture != PostureSitting {
		t.Errorf("seduto riconosciuto come %q (cosce %v)", PostureNames[m.Posture], m.ThighDeg)
	}
	if j := m.Joints[JKneeR]; !j.OK || math.Abs(j.Deg-92.5) > 0.5 {
		t.Errorf("ginocchio dx piegato: %+v", j)
	}
	if m.ForwardDeg >= 0 {
		t.Errorf("busto all'indietro di %v°", m.ForwardDeg)
	}

	lying := pose(map[int][3]float64{
		LeftShoulder: {-0.5, 0, 0.2}, RightShoulder: {-0.5, 0, -0.2}, LeftHip: {0, 0, 0.1}, RightHip: {0, 0, -0.1},
	})
	if m := Measure(lying, 0); m.Posture != PostureLying {
		t.Errorf("sdraiato riconosciuto come %q", PostureNames[m.Posture])
	}
	// punti coperti: niente postura
	hidden := pose(map[int][3]float64{LeftShoulder: {0.2, -0.5, 0}, RightShoulder: {-0.2, -0.5, 0}})
	if m := Measure(hidden, 0); m.Posture != PostureUnknown {
		t.Errorf("senza fianchi: %q", PostureNames[m.Posture])
	}
	up := pose(map[int][3]float64{LeftShoulder: {0.2, -0.5, 0}, RightShoulder: {-0.2, -0.5, 0}})
	up.Mode, up.HasWorld = UpperBody, false
	if m := Measure(up, 1000); m.Posture != PostureUpper || !m.DistAssume || math.Abs(m.DistM-1000*ShoulderWidthM/80) > 1e-9 {
		t.Errorf("mezzo busto: %q, distanza %v (ipotizzata %v)", PostureNames[m.Posture], m.DistM, m.DistAssume)
	}
}

func TestSkeleton(t *testing.T) {
	if Side(LeftShoulder) != -1 || Side(RightKnee) != 1 || Side(Nose) != 0 || Side(LeftEar) != -1 {
		t.Error("lati dei punti sbagliati")
	}
	for _, b := range Bones {
		if b[0] >= NumLandmarks || b[1] >= NumLandmarks {
			t.Fatalf("segmento %v fuori dai punti", b)
		}
	}
	if (Bone{LeftShoulder, RightShoulder}).Part() != PartTorso || (Bone{LeftElbow, LeftWrist}).Part() != -1 ||
		(Bone{0, 4}).Part() != PartFace {
		t.Error("parti dello scheletro sbagliate")
	}
}
