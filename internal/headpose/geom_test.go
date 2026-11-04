package headpose

import (
	"math"
	"math/rand"
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// display area reale del 4C di prova (sistema del tracker, mm)
var testArea = ttp.DisplayArea{
	TopLeft:    [3]float64{-349.1, 386.2, 132.5},
	TopRight:   [3]float64{348.9, 386.2, 132.5},
	BottomLeft: [3]float64{-349.1, 16.9, -1.9},
}

// testCamera: camera vicino all'origine del tracker, inclinata verso l'alto.
func testCamera() *Camera {
	R := rodrigues(vec3{-18 * math.Pi / 180, 0.02, 0.01}).mul(mat3{{-1, 0, 0}, {0, -1, 0}, {0, 0, 1}})
	return &Camera{F: 710, CX: 326, CY: 236, R: R, C: vec3{1.5, 4, -3}, W: 640, H: 480}
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestProcrustesRecoversSimilarity(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	R := rodrigues(vec3{0.3, -0.5, 0.2})
	src := make([]vec3, 20)
	dst := make([]vec3, 20)
	w := make([]float64, 20)
	for i := range src {
		src[i] = vec3{r.NormFloat64() * 30, r.NormFloat64() * 30, r.NormFloat64() * 30}
		dst[i] = add(scale(R.apply(src[i]), 2.5), vec3{10, -4, 600})
		w[i] = 0.1 + r.Float64()
	}
	gR, s, tr := procrustes(src, dst, w)
	if angleBetween(gR, R) > 1e-9 || !near(s, 2.5, 1e-9) || norm(sub(tr, vec3{10, -4, 600})) > 1e-6 {
		t.Fatalf("R err %g s %v t %v", angleBetween(gR, R), s, tr)
	}
}

func TestEulerRoundTrip(t *testing.T) {
	for _, a := range [][3]float64{{0, 0, 0}, {0.3, 0, 0}, {0, 0.4, 0}, {0, 0, -0.2}, {0.2, -0.5, 0.15}, {-0.4, 0.3, -0.3}} {
		p, y, r := Euler(fromEuler(a[0], a[1], a[2]))
		if !near(p, a[0], 1e-12) || !near(y, a[1], 1e-12) || !near(r, a[2], 1e-12) {
			t.Fatalf("%v → %v %v %v", a, p, y, r)
		}
	}
	// convenzioni: pitch positivo = il viso guarda in alto (+Y), yaw positivo
	// = verso la sinistra dell'utente (−X)
	fwd := fromEuler(0.3, 0, 0).apply(vec3{0, 0, -1})
	if fwd[1] <= 0 {
		t.Fatalf("positive pitch looks %v", fwd)
	}
	fwd = fromEuler(0, 0.3, 0).apply(vec3{0, 0, -1})
	if fwd[0] >= 0 {
		t.Fatalf("positive yaw looks %v", fwd)
	}
}

func TestRotvecRoundTrip(t *testing.T) {
	for _, v := range []vec3{{0, 0, 0}, {0.1, -0.2, 0.3}, {0, 0, math.Pi}, {math.Pi * 0.999, 0, 0}, {-1, 2, 0.5}} {
		R := rodrigues(v)
		if a := angleBetween(rodrigues(rotvec(R)), R); a > 1e-6 {
			t.Fatalf("%v: error %g", v, a)
		}
	}
}

// synthFace proietta il modello canonico in posa nota (rotazione nel sistema
// dello schermo, posizione del centro del modello in mm dal centro dello
// schermo) e restituisce landmark come quelli del modello: x, y in pixel e z
// relativa nella scala di x.
func synthFace(cam *Camera, area ttp.DisplayArea, rot mat3, posDisplay vec3) (*Landmarks, vec3, vec3) {
	toD := displayBasis(area)
	origin := scale(add(vec3(area.TopRight), vec3(area.BottomLeft)), 0.5)
	toTracker := func(pd vec3) vec3 { return add(origin, toD.t().apply(pd)) }
	center := cam.toCamera(toTracker(posDisplay))
	var lm Landmarks
	place := func(m vec3) vec3 { // mm canonici → tracker
		return toTracker(add(posDisplay, rot.mul(headToScreen).apply(m)))
	}
	// profondità del viso (media pesata dei punti): la z del modello è nella
	// scala di x a quella profondità
	var zf, ws float64
	for _, p := range procrustesBasis {
		zf += p.weight * cam.toCamera(place(vec3{p.x * 10, p.y * 10, p.z * 10}))[2]
		ws += p.weight
	}
	zf /= ws
	for _, p := range procrustesBasis {
		pc := cam.toCamera(place(vec3{p.x * 10, p.y * 10, p.z * 10}))
		lm[p.id] = [3]float64{cam.F*pc[0]/pc[2] + cam.CX, cam.F*pc[1]/pc[2] + cam.CY, (pc[2] - center[2]) * cam.F / zf}
	}
	// centri corneali approssimati: 1 cm dietro gli angoli interni ed esterni
	eye := func(a, b int) vec3 {
		var pa, pb canonicalPoint
		for _, p := range procrustesBasis {
			if p.id == a {
				pa = p
			}
			if p.id == b {
				pb = p
			}
		}
		return place(vec3{(pa.x + pb.x) * 5, (pa.y + pb.y) * 5, (pa.z+pb.z)*5 - 10})
	}
	return &lm, eye(263, 362), eye(33, 133) // sinistro, destro del soggetto
}

func TestHeadPoseFromSyntheticLandmarks(t *testing.T) {
	cam := testCamera()
	toD := displayBasis(testArea)
	for _, c := range []struct {
		p, y, r float64
		pos     vec3
	}{
		{0, 0, 0, vec3{0, 80, 620}},
		{0.25, 0, 0, vec3{0, 80, 620}},
		{-0.3, 0, 0, vec3{40, 60, 580}},
		{0.1, 0.35, -0.1, vec3{-90, 120, 700}},
		{-0.15, -0.3, 0.2, vec3{120, 30, 520}},
	} {
		lm, _, _ := synthFace(cam, testArea, fromEuler(c.p, c.y, c.r), c.pos)
		Rc, _, ok := headInCamera(lm, cam)
		if !ok {
			t.Fatal("pose failed")
		}
		got := headInDisplay(Rc, cam, toD)
		if !near(got.Pitch, c.p, 0.002) || !near(got.Yaw, c.y, 0.002) || !near(got.Roll, c.r, 0.002) {
			t.Errorf("want p/y/r %.1f/%.1f/%.1f°, got %.2f/%.2f/%.2f°", deg(c.p), deg(c.y), deg(c.r),
				deg(got.Pitch), deg(got.Yaw), deg(got.Roll))
		}
	}
}

// synthSamples genera campioni di calibrazione con teste in posizioni e
// orientamenti casuali; mirror simula un dispositivo con immagine speculare.
func synthSamples(t *testing.T, cam *Camera, n int, noise float64, mirror bool) []CalibSample {
	r := rand.New(rand.NewSource(7))
	var out []CalibSample
	for len(out) < n {
		pos := vec3{r.Float64()*240 - 120, r.Float64()*160 - 20, 520 + r.Float64()*220}
		rot := fromEuler(r.NormFloat64()*0.15, r.NormFloat64()*0.25, r.NormFloat64()*0.08)
		_, left, right := synthFace(cam, testArea, rot, pos)
		ul, vl, ok1 := cam.Project(left)
		ur, vr, ok2 := cam.Project(right)
		if !ok1 || !ok2 || ul < 0 || ur > 640 || vl < 0 || vl > 480 || vr < 0 || vr > 480 {
			continue
		}
		s := CalibSample{Left: left, Right: right,
			Iris468: [2]float64{ur + r.NormFloat64()*noise, vr + r.NormFloat64()*noise},
			Iris473: [2]float64{ul + r.NormFloat64()*noise, vl + r.NormFloat64()*noise}}
		if mirror {
			// il dispositivo invia l'immagine ribaltata: il modello vede a
			// sinistra l'occhio sinistro dell'utente e lo chiama "destro"
			s.Iris468, s.Iris473 = [2]float64{640 - s.Iris473[0], s.Iris473[1]}, [2]float64{640 - s.Iris468[0], s.Iris468[1]}
		}
		out = append(out, s)
	}
	return out
}

func TestCalibrateRecoversCamera(t *testing.T) {
	truth := testCamera()
	for _, mirror := range []bool{false, true} {
		samples := synthSamples(t, truth, 300, 0.8, mirror)
		cam, q, err := Calibrate(samples, 640, 480)
		if err != nil {
			t.Fatal(err)
		}
		rotErr := deg(angleBetween(cam.R, truth.R))
		t.Logf("mirror=%v: f %.1f (%.1f) pp (%.1f,%.1f) C %.1f rot err %.3f° rms %.2f px σ %.3f° depth %.0f mm",
			mirror, cam.F, truth.F, cam.CX, cam.CY, cam.C, rotErr, q.RMSPx, q.SigmaDeg, q.DepthMM)
		if cam.Mirror != mirror {
			t.Fatalf("mirror detected %v, want %v", cam.Mirror, mirror)
		}
		if rotErr > 0.3 || math.Abs(cam.F-truth.F)/truth.F > 0.03 || q.RMSPx > 1.5 {
			t.Fatalf("calibration off")
		}
	}
}

// fromEuler è l'inverso di Euler.
func fromEuler(pitch, yaw, roll float64) mat3 {
	sp, cp := math.Sincos(pitch)
	sy, cy := math.Sincos(yaw)
	sr, cr := math.Sincos(roll)
	Ry := mat3{{cy, 0, sy}, {0, 1, 0}, {-sy, 0, cy}}
	Rx := mat3{{1, 0, 0}, {0, cp, -sp}, {0, sp, cp}}
	Rz := mat3{{cr, -sr, 0}, {sr, cr, 0}, {0, 0, 1}}
	return Ry.mul(Rx).mul(Rz)
}

// angleBetween è l'angolo (rad) tra due rotazioni.
func angleBetween(a, b mat3) float64 {
	return norm(rotvec(a.t().mul(b)))
}
