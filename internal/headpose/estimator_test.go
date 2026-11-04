package headpose

import (
	"log/slog"
	"math"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
)

// fakeLandmarker restituisce i landmark di una testa sintetica in posa nota,
// proiettata con la camera "vera": sostituisce il modello di MediaPipe.
type fakeLandmarker struct {
	searches, detects int
	cam               *Camera
	truth             func(us int64) (rot mat3, pos vec3)
	noise             float64
	rng               *rand.Rand
}

func (f *fakeLandmarker) landmarks(us int64, out *Landmarks) {
	// come sul 4C: l'immagine mostra la testa com'era imageGazeLagUs prima
	// del suo timestamp
	rot, pos := f.truth(us - imageGazeLagUs)
	lm, left, right := synthFace(f.cam, testArea, rot, pos)
	*out = *lm
	for i := range out {
		if out[i] == ([3]float64{}) { // punti non sintetizzati: al centro del viso
			out[i] = lm[lmNoseTip]
		}
	}
	ul, vl, _ := f.cam.Project(left)
	ur, vr, _ := f.cam.Project(right)
	out[lmRightIris] = [3]float64{ur, vr, 0}
	out[lmLeftIris] = [3]float64{ul, vl, 0}
	for i := range out {
		out[i][0] += f.rng.NormFloat64() * f.noise
		out[i][1] += f.rng.NormFloat64() * f.noise
	}
}

func (f *fakeLandmarker) Detect(img *irimg.Frame, c Crop, out *Landmarks) float64 {
	f.detects++
	f.landmarks(img.DeviceUs, out)
	return 0.99
}
func (f *fakeLandmarker) Search(img *irimg.Frame, out *Landmarks) (Crop, float64) {
	f.searches++
	f.landmarks(img.DeviceUs, out)
	return CropFromLandmarks(out), 0.99
}
func (f *fakeLandmarker) Refine(img *irimg.Frame, c Crop, out *Landmarks, iter int) (Crop, float64) {
	f.landmarks(img.DeviceUs, out)
	return CropFromLandmarks(out), 0.99
}
func (f *fakeLandmarker) MACs() int64 { return 0 }

// eyesTracker restituisce i centri corneali sintetici nel sistema del tracker.
func eyesTracker(cam *Camera, rot mat3, pos vec3) (l, r vec3) {
	_, l, r = synthFace(cam, testArea, rot, pos)
	return
}

func TestEstimatorCalibratesAndEstimatesPitch(t *testing.T) {
	truthCam := testCamera()
	// la testa si muove su e giù, avanti e indietro e di lato, e annuisce
	truth := func(us int64) (mat3, vec3) {
		s := float64(us) / 1e6
		pos := vec3{110 * math.Sin(0.9*s), 50 + 60*math.Sin(0.6*s+1), 620 + 110*math.Sin(0.35*s)}
		return fromEuler(0.25*math.Sin(1.3*s), 0.3*math.Sin(0.5*s), 0.08*math.Sin(0.7*s)), pos
	}
	fake := &fakeLandmarker{cam: truthCam, truth: truth, noise: 0.5, rng: rand.New(rand.NewSource(5))}
	dir := t.TempDir()
	e := newEstimator(Config{CameraPath: filepath.Join(dir, "camera.json")}, slog.New(slog.NewTextHandler(testWriter{t}, nil)), fake)
	e.SetDevice("IS404-TEST", &testArea)

	frame := &irimg.Frame{W: 640, H: 480, Pix: make([]byte, 640*480)}
	var calibratedAt int64
	var maxErr float64
	checked := 0
	for k := 0; k < 30*60; k++ { // un minuto a 30 Hz
		us := int64(k) * 33_333
		for g := 0; g < 3; g++ { // gaze a 90 Hz
			gu := us - 22_222 + int64(g)*11_111
			rot, pos := truth(gu)
			l, r := eyesTracker(truthCam, rot, pos)
			var s gaze.Sample
			s.DeviceTimeUs = gu
			s.Left.OriginMM = [3]float32{float32(l[0]), float32(l[1]), float32(l[2])}
			s.Right.OriginMM = [3]float32{float32(r[0]), float32(r[1]), float32(r[2])}
			s.Left.OriginValid, s.Right.OriginValid = true, true
			e.Observe(&s)
		}
		frame.DeviceUs = us
		e.process(frame)
		if e.calibBusy.Load() {
			e.adoptCalibration(<-e.calibDone) // nel driver lo fa la goroutine di lavoro
			if e.wcam != nil && calibratedAt == 0 {
				calibratedAt = us
			}
		}
		res := e.Last()
		if !res.Valid {
			continue
		}
		rot, _ := truth(us - imageGazeLagUs)
		p, _, _ := Euler(rot)
		if err := math.Abs(res.RawPitch - p); err > maxErr {
			maxErr = err
		}
		checked++
	}
	if calibratedAt == 0 {
		t.Fatal("camera never calibrated")
	}
	t.Logf("calibrated after %.1f s; %d frames checked, max raw pitch error %.2f°; camera rms %.2f px σ %.2f°",
		float64(calibratedAt)/1e6, checked, deg(maxErr), e.wq.RMSPx, e.wq.SigmaDeg)
	if checked < 500 || deg(maxErr) > 1.0 {
		t.Fatalf("pitch error %.2f° over %d frames", deg(maxErr), checked)
	}

	// il pitch viene applicato ai campioni vicini nel tempo, non a quelli vecchi
	last := e.Last()
	s := gaze.Sample{DeviceTimeUs: last.DeviceUs + 20_000}
	e.Observe(&s)
	if !s.Head.RotationValid[0] || s.Head.RotationRad[0] != float32(last.Pitch) || s.Head.Source != gaze.HeadDerived {
		t.Fatalf("pitch not applied: %+v", s.Head)
	}
	old := gaze.Sample{DeviceTimeUs: last.DeviceUs + int64(time.Second/time.Microsecond)}
	e.Observe(&old)
	if old.Head.RotationValid[0] {
		t.Fatal("stale pitch applied")
	}

	// la calibrazione salvata viene ricaricata per lo stesso dispositivo
	cam, f, err := LoadCamera(filepath.Join(dir, "camera.json"), "IS404-TEST")
	if err != nil {
		t.Fatal(err)
	}
	if deg(angleBetween(cam.R, truthCam.R)) > 0.5 || f.Pairs == 0 {
		t.Fatalf("saved camera off by %.2f°", deg(angleBetween(cam.R, truthCam.R)))
	}
	if _, _, err := LoadCamera(filepath.Join(dir, "camera.json"), "OTHER"); err == nil {
		t.Fatal("calibration of another device accepted")
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func TestCropFromEyesMatchesMediaPipeProportions(t *testing.T) {
	// ritaglio MediaPipe sulla foto di prova: iridi (425,206) e (527,205),
	// ritaglio centrato in (471,263) con lato 414
	cam := &Camera{F: 1000, CX: 0, CY: 0, R: ident3, W: 960, H: 1199}
	// punti 3D a 1 m che si proiettano sulle iridi
	c, ok := cropFromEyes(cam, vec3{527, 205, 1000}, vec3{425, 206, 1000})
	if !ok {
		t.Fatal("no crop")
	}
	if math.Abs(c.CX-471) > 10 || math.Abs(c.CY-263) > 10 || math.Abs(c.Side-414) > 20 {
		t.Fatalf("crop %+v", c)
	}
}

// Dopo una ricerca lenta le immagini intermedie vengono scartate: il frame
// elaborato dopo deve ripartire dai landmark precedenti, non da un'altra
// ricerca (bug visto sul 4C: una ricerca dietro l'altra).
func TestTrackingSurvivesSlowSearch(t *testing.T) {
	truthCam := testCamera()
	truth := func(us int64) (mat3, vec3) { return ident3, vec3{0, 60, 650} }
	fake := &fakeLandmarker{cam: truthCam, truth: truth, rng: rand.New(rand.NewSource(1))}
	e := newEstimator(Config{}, slog.New(slog.NewTextHandler(testWriter{t}, nil)), fake)
	frame := &irimg.Frame{W: 640, H: 480, Pix: make([]byte, 640*480)}
	for _, us := range []int64{1_000_000, 1_450_000, 1_900_000, 2_350_000} { // un frame ogni 450 ms
		frame.DeviceUs = us
		e.process(frame)
	}
	if fake.searches != 1 {
		t.Fatalf("%d searches, want 1 (then tracking)", fake.searches)
	}
}
