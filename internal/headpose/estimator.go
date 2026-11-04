// Package headpose stima la rotazione della testa (in particolare il pitch,
// che i due soli centri corneali non permettono di osservare) dalle immagini
// IR dello stream 0x501.
//
// Catena: ritaglio del viso → landmark MediaPipe (interprete ONNX in Go
// puro, package nn) → fit del modello canonico del viso nel sistema della
// camera → sistema del tracker (camera calibrata) → sistema dello schermo
// (display area) → angoli di Eulero come la head_pose dello Stream Engine.
//
// La camera si calibra da sola: confronta i centri delle iridi trovati nelle
// immagini con i centri corneali 3D del tracker, e salva il risultato.
package headpose

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// Config configura lo stimatore.
type Config struct {
	ModelPath  string        // modello ONNX dei landmark
	CameraPath string        // file della calibrazione della camera (letto e scritto)
	Workers    int           // goroutine dell'inferenza (default 1: più goroutine guadagnano poco e costano CPU)
	MaxRate    float64       // inferenze al secondo al massimo (0 = una per immagine, 30 Hz)
	MaxAge     time.Duration // età massima del pitch applicato a un campione (default 150 ms)
	DebugDir   string        // se non vuoto, salva immagini annotate
	DebugEvery time.Duration // intervallo tra le immagini di debug (default 2 s)
	MinCutoff  float64       // filtro 1€: taglio a testa ferma, Hz (default 1)
	Beta       float64       // filtro 1€: crescita del taglio con la velocità (default 0.8)

	// ManualCalibration: la camera si calibra solo su richiesta
	// (CalibrateNow), come fa l'applicazione headpose-calibrate.
	ManualCalibration bool
	// RecordDir: se non vuoto, registra landmark e occhi (frames.jsonl,
	// gaze.jsonl) per l'analisi della calibrazione.
	RecordDir string
	// UserPath: file con lo zero personale del pitch (letto e scritto).
	UserPath string
}

// Result è l'ultima stima, nel sistema dello schermo.
type Result struct {
	DeviceUs         int64 // timestamp dell'immagine
	Pitch, Yaw, Roll float64
	RawPitch         float64 // prima del filtro
	Valid            bool
	Score            float64 // probabilità del viso
	InferenceMs      float64
	CameraCalibrated bool
}

type imgMsg struct {
	buf  []byte
	rxNs int64
}

type eyeObs struct {
	us     int64
	l, r   vec3
	lv, rv bool
	yaw    float64 // yaw e roll ricavati dagli occhi (per il confronto)
	roll   float64
	hv     bool
}

// Estimator riceve le immagini e i campioni gaze dalla pipeline del tracker.
type Estimator struct {
	cfg Config
	log *slog.Logger
	det landmarker

	free chan []byte
	work chan imgMsg
	done chan struct{}
	wg   sync.WaitGroup

	mu        sync.Mutex
	ring      [64]eyeObs
	ringN     int
	away      bool // lo stream presence dice che davanti al sensore non c'è nessuno
	result    Result
	area      ttp.DisplayArea
	hasArea   bool
	serial    string
	cam       *Camera // calibrazione caricata da SetDevice, da passare al worker
	camQ      CalibQuality
	camNew    bool
	diff      diffStats
	offset    float64 // zero personale del pitch (rad), sottratto alla stima
	hasOffset bool
	cov       Coverage
	prev      Preview // ultima immagine elaborata, per l'interfaccia

	ctl chan func() // comandi da eseguire nella goroutine di lavoro
	rec *recorder

	// solo goroutine di lavoro
	wcam       *Camera
	wq         CalibQuality
	track      bool
	lastLM     Landmarks
	lm         Landmarks // landmark del frame in elaborazione (niente allocazioni per frame)
	lastUs     int64
	lastSearch int64 // µs del dispositivo
	searchGap  int64 // attesa prima della prossima ricerca, cresce se fallisce
	lastRun    time.Time
	lastDebug  time.Time
	filt       oneEuro
	row        ttp.Row
	frame      irimg.Frame
	samples    []CalibSample
	voxels     map[[3]int][]int
	newVoxels  int
	calibBusy  atomic.Bool
	calibDone  chan calibOut
	lastCalib  int64 // µs del dispositivo
	rng        *rand.Rand

	// statistiche
	Frames, Dropped, Processed, Inferences, Searches, Tracked atomic.Uint64
	infNs                                                     atomic.Int64
}

type calibOut struct {
	cam *Camera
	q   CalibQuality
	err error
	n   int
}

type diffStats struct {
	n               int
	sumYaw, sumRoll float64
	sqYaw, sqRoll   float64
}

// landmarker è il rilevatore dei landmark (Detector; un finto nei test).
type landmarker interface {
	Detect(img *irimg.Frame, c Crop, out *Landmarks) float64
	Search(img *irimg.Frame, out *Landmarks) (Crop, float64)
	Refine(img *irimg.Frame, c Crop, out *Landmarks, iter int) (Crop, float64)
	MACs() int64
}

// New carica il modello e avvia la goroutine di lavoro.
func New(cfg Config, log *slog.Logger) (*Estimator, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	det, err := NewDetector(cfg.ModelPath, cfg.Workers)
	if err != nil {
		return nil, err
	}
	e := newEstimator(cfg, log, det)
	log.Info("head pose model loaded", "path", cfg.ModelPath, "mmac", det.MACs()/1_000_000, "workers", cfg.Workers)
	if !det.Verified {
		log.Warn("head pose: the face model is not the tested one (SHA-256 differs)", "want_sha256", ModelSHA256)
	}
	e.wg.Add(1)
	go e.loop()
	return e, nil
}

func newEstimator(cfg Config, log *slog.Logger, det landmarker) *Estimator {
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 150 * time.Millisecond
	}
	if cfg.DebugEvery <= 0 {
		cfg.DebugEvery = 2 * time.Second
	}
	if cfg.MinCutoff <= 0 {
		cfg.MinCutoff = 1
	}
	if cfg.Beta <= 0 {
		cfg.Beta = 0.8
	}
	e := &Estimator{
		cfg: cfg, log: log, det: det,
		free:      make(chan []byte, 2),
		work:      make(chan imgMsg, 1),
		done:      make(chan struct{}),
		voxels:    map[[3]int][]int{},
		calibDone: make(chan calibOut, 1),
		ctl:       make(chan func(), 8),
		filt:      newOneEuro(cfg.MinCutoff, cfg.Beta),
		rng:       rand.New(rand.NewSource(1)),
	}
	for i := 0; i < cap(e.free); i++ {
		e.free <- make([]byte, 0, 320*1024)
	}
	if cfg.UserPath != "" {
		if off, err := LoadPitchOffset(cfg.UserPath); err == nil {
			e.offset, e.hasOffset = off, true
			log.Info("head pose: personal pitch zero loaded", "offset_deg", round1(deg(off)), "path", cfg.UserPath)
		}
	}
	if cfg.RecordDir != "" {
		r, err := newRecorder(cfg.RecordDir)
		if err != nil {
			log.Warn("head pose: recording disabled", "err", err)
		} else {
			e.rec = r
		}
	}
	return e
}

// Close ferma la goroutine di lavoro.
func (e *Estimator) Close() {
	close(e.done)
	e.wg.Wait()
	if d, ok := e.det.(*Detector); ok {
		d.model.Close()
	}
	if e.rec != nil {
		e.rec.close()
	}
}

// SetDevice comunica numero di serie e display area (dopo l'handshake o un
// cambio di display area). Carica la calibrazione della camera salvata.
func (e *Estimator) SetDevice(serial string, area *ttp.DisplayArea) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if area != nil {
		e.area, e.hasArea = *area, true
	}
	if serial == e.serial {
		return
	}
	e.serial = serial
	if e.cfg.RecordDir != "" {
		data, _ := json.Marshal(map[string]any{"serial": serial, "display_area": area})
		_ = os.WriteFile(filepath.Join(e.cfg.RecordDir, "session.json"), append(data, '\n'), 0o644)
	}
	if e.cfg.CameraPath == "" {
		return
	}
	cam, f, err := LoadCamera(e.cfg.CameraPath, serial)
	if err != nil {
		e.log.Info("head pose: no saved camera calibration, it will be estimated while you use the tracker", "path", e.cfg.CameraPath, "reason", err)
		return
	}
	e.cam, e.camNew = cam, true
	e.camQ = CalibQuality{RMSPx: f.RMSPx, SigmaDeg: f.SigmaDeg, Pairs: f.Pairs, DepthMM: f.DepthMM, Mirror: f.Mirror}
	e.log.Info("head pose: camera calibration loaded", "path", e.cfg.CameraPath, "focal_px", round1(f.F),
		"rms_px", round2(f.RMSPx), "sigma_deg", round2(f.SigmaDeg), "mirror", f.Mirror)
}

// OfferImage consegna un payload dello stream 0x501. Chiamata dalla goroutine
// framer: non blocca mai; se lo stimatore è occupato l'immagine è scartata.
func (e *Estimator) OfferImage(payload []byte, rxNs int64) {
	e.Frames.Add(1)
	var buf []byte
	select {
	case buf = <-e.free:
	default:
		e.Dropped.Add(1)
		return
	}
	buf = append(buf[:0], payload...)
	select {
	case e.work <- imgMsg{buf: buf, rxNs: rxNs}:
	default:
		e.free <- buf
		e.Dropped.Add(1)
	}
}

// Observe registra gli occhi di un campione gaze e gli applica l'ultimo
// pitch disponibile. Chiamata dalla goroutine emitter prima dei sink.
func (e *Estimator) Observe(s *gaze.Sample) {
	e.mu.Lock()
	o := &e.ring[e.ringN%len(e.ring)]
	e.ringN++
	o.us = s.DeviceTimeUs
	o.lv, o.rv = s.Left.OriginValid, s.Right.OriginValid
	o.l = vec3{float64(s.Left.OriginMM[0]), float64(s.Left.OriginMM[1]), float64(s.Left.OriginMM[2])}
	o.r = vec3{float64(s.Right.OriginMM[0]), float64(s.Right.OriginMM[1]), float64(s.Right.OriginMM[2])}
	o.hv = s.Head.RotationValid[1] && s.Head.RotationValid[2]
	o.yaw, o.roll = float64(s.Head.RotationRad[1]), float64(s.Head.RotationRad[2])
	e.away = s.Presence == gaze.PresenceAway
	res := e.result
	e.mu.Unlock()
	if e.rec != nil {
		e.rec.gaze(s)
	}
	if !res.Valid {
		return
	}
	age := s.DeviceTimeUs - res.DeviceUs
	if age < -e.cfg.MaxAge.Microseconds() || age > e.cfg.MaxAge.Microseconds() {
		return
	}
	if s.Head.Source == gaze.HeadNone {
		s.Head.Source = gaze.HeadDerived
	}
	s.Head.RotationRad[0] = float32(res.Pitch)
	s.Head.RotationValid[0] = true
}

// Last restituisce l'ultima stima.
func (e *Estimator) Last() Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result
}

func (e *Estimator) loop() {
	defer e.wg.Done()
	for {
		select {
		case <-e.done:
			return
		case out := <-e.calibDone:
			e.adoptCalibration(out)
		case fn := <-e.ctl:
			fn()
		case msg := <-e.work:
			err := irimg.DecodePayload(msg.buf, &e.row, &e.frame)
			e.frame.HostNs = msg.rxNs
			e.free <- msg.buf
			if err != nil {
				e.log.Warn("head pose: image decode failed", "err", err)
				continue
			}
			// limite di frequenza con margine: le immagini arrivano con un po' di
			// jitter attorno ai 33 ms, e un limite esatto di 30 Hz ne scarterebbe
			// una su tre
			if e.cfg.MaxRate > 0 && time.Since(e.lastRun) < time.Duration(0.8*float64(time.Second)/e.cfg.MaxRate) {
				continue
			}
			e.lastRun = time.Now()
			e.Processed.Add(1)
			e.process(&e.frame)
		}
	}
}

// eyesAt interpola le posizioni degli occhi all'istante us.
func (e *Estimator) eyesAt(us int64) (l, r vec3, ok bool, yaw, roll float64, hv bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := min(e.ringN, len(e.ring))
	var before, after *eyeObs
	for i := 0; i < n; i++ {
		o := &e.ring[(e.ringN-1-i)%len(e.ring)]
		if o.us <= us && (before == nil || o.us > before.us) {
			before = o
		}
		if o.us >= us && (after == nil || o.us < after.us) {
			after = o
		}
	}
	switch {
	case before == nil && after == nil:
		return
	case before == nil:
		before = after
	case after == nil:
		after = before
	}
	if us-before.us > 15_000 || after.us-us > 15_000 || !before.lv || !before.rv || !after.lv || !after.rv {
		return
	}
	t := 0.0
	if after.us > before.us {
		t = float64(us-before.us) / float64(after.us-before.us)
	}
	lerp := func(a, b vec3) vec3 { return add(scale(a, 1-t), scale(b, t)) }
	return lerp(before.l, after.l), lerp(before.r, after.r), true, before.yaw, before.roll, before.hv && after.hv
}

func flipHorizontal(f *irimg.Frame) {
	for y := 0; y < f.H; y++ {
		row := f.Pix[y*f.W : (y+1)*f.W]
		for i, j := 0, len(row)-1; i < j; i, j = i+1, j-1 {
			row[i], row[j] = row[j], row[i]
		}
	}
}

// cropFromEyes ricava il ritaglio del viso dalle posizioni degli occhi
// proiettate nell'immagine (proporzioni misurate sui ritagli di MediaPipe).
func cropFromEyes(cam *Camera, l, r vec3) (Crop, bool) {
	ul, vl, ok1 := cam.Project(l)
	ur, vr, ok2 := cam.Project(r)
	if !ok1 || !ok2 {
		return Crop{}, false
	}
	// immagine non speculare: l'occhio destro dell'utente sta a sinistra
	dx, dy := ul-ur, vl-vr
	iod := math.Hypot(dx, dy)
	if iod < 10 {
		return Crop{}, false
	}
	a := math.Atan2(dy, dx)
	sin, cos := math.Sincos(a)
	mx, my := (ul+ur)/2, (vl+vr)/2
	return Crop{CX: mx - 0.55*iod*sin, CY: my + 0.55*iod*cos, Side: 4 * iod, Angle: a}, true
}

func (e *Estimator) process(f *irimg.Frame) {
	e.mu.Lock()
	if e.camNew {
		e.wcam, e.wq, e.camNew = e.cam, e.camQ, false
		e.track = false
	}
	area, hasArea, away := e.area, e.hasArea, e.away
	e.mu.Unlock()
	cam := e.wcam
	mirror := cam != nil && cam.Mirror
	if mirror {
		flipHorizontal(f)
	}
	l, r, eyesOK, eyeYaw, eyeRoll, eyeHV := e.eyesAt(f.DeviceUs - imageGazeLagUs)

	t0 := time.Now()
	lm := &e.lm
	var crop Crop
	score := 0.0
	have := false
	switch {
	// Si riusa il ritaglio dell'ultimo frame riuscito anche se è passato un
	// po' di tempo: dopo una ricerca (centinaia di ms) le immagini intermedie
	// sono state scartate, e senza questo margine ogni ricerca ne
	// innescherebbe un'altra. Se il viso si è spostato troppo, il punteggio
	// cade e si torna alla ricerca.
	case e.track && f.DeviceUs-e.lastUs < trackWindowUs && f.DeviceUs >= e.lastUs:
		crop, have = CropFromLandmarks(&e.lastLM), true
	case cam != nil && eyesOK:
		crop, have = cropFromEyes(cam, l, r)
	}
	if have {
		score = e.det.Detect(f, crop, lm)
		e.Inferences.Add(1)
		if score >= minFaceScore && !e.track {
			crop, score = e.det.Refine(f, CropFromLandmarks(lm), lm, 0)
			e.Inferences.Add(1)
		}
	}
	// La ricerca a tutta immagine costa una trentina di inferenze: mai quando
	// il tracker dice che non c'è nessuno, e sempre più di rado se fallisce.
	if score < minFaceScore && !away && (f.DeviceUs-e.lastSearch > e.searchGap || f.DeviceUs < e.lastSearch) {
		e.lastSearch = f.DeviceUs
		crop, score = e.det.Search(f, lm)
		e.Searches.Add(1)
		if score < minFaceScore {
			e.searchGap = min(max(2*e.searchGap, 1_000_000), 5_000_000)
		}
	}
	if score >= minFaceScore {
		e.searchGap = 0
	}
	infMs := float64(time.Since(t0).Microseconds()) / 1000
	e.infNs.Add(time.Since(t0).Nanoseconds())

	res := Result{DeviceUs: f.DeviceUs, Score: score, InferenceMs: infMs, CameraCalibrated: cam != nil}
	if score < minFaceScore {
		e.track = false
		e.filt.reset()
		e.publish(res, false, 0, 0, false)
		e.publishPreview(f, nil, crop, res)
		e.debugImage(f, nil, crop, cam, l, r, eyesOK, res)
		return
	}
	e.Tracked.Add(1)
	e.track, e.lastLM, e.lastUs = true, *lm, f.DeviceUs

	if eyesOK && score > 0.9 {
		e.addSample(lm, l, r, mirror, f.W)
	}
	e.maybeCalibrate(f.W, f.H, f.DeviceUs)

	if e.rec != nil {
		e.rec.frame(f.DeviceUs, score, lm, mirror, f.W)
	}
	if cam == nil || !hasArea {
		e.publish(res, false, 0, 0, false)
		e.publishPreview(f, lm, crop, res)
		e.debugImage(f, lm, crop, cam, l, r, eyesOK, res)
		return
	}
	Rc, _, ok := headInCamera(lm, cam)
	if !ok {
		e.publish(res, false, 0, 0, false)
		e.publishPreview(f, lm, crop, res)
		return
	}
	hr := headInDisplay(Rc, cam, displayBasis(area))
	e.mu.Lock()
	off := e.offset
	e.mu.Unlock()
	res.RawPitch = hr.Pitch
	res.Pitch = e.filt.filter(hr.Pitch, f.DeviceUs) - off
	res.Yaw, res.Roll, res.Valid = hr.Yaw, hr.Roll, true
	e.publish(res, eyeHV, eyeYaw, eyeRoll, true)
	e.publishPreview(f, lm, crop, res)
	e.debugImage(f, lm, crop, cam, l, r, eyesOK, res)
}

func (e *Estimator) publish(res Result, cmp bool, eyeYaw, eyeRoll float64, ok bool) {
	e.mu.Lock()
	e.result = res
	if cmp && ok {
		dy, dr := res.Yaw-eyeYaw, res.Roll-eyeRoll
		e.diff.n++
		e.diff.sumYaw += dy
		e.diff.sumRoll += dr
		e.diff.sqYaw += dy * dy
		e.diff.sqRoll += dr * dr
	}
	e.mu.Unlock()
}

// addSample aggiunge una coppia iridi ↔ occhi 3D al serbatoio della
// calibrazione, distribuito su celle di 25 mm (al più 20 campioni per cella,
// 3000 in tutto): così una posizione tenuta a lungo non pesa più delle altre.
func (e *Estimator) addSample(lm *Landmarks, l, r vec3, mirror bool, w int) {
	i468 := [2]float64{lm[lmRightIris][0], lm[lmRightIris][1]}
	i473 := [2]float64{lm[lmLeftIris][0], lm[lmLeftIris][1]}
	if mirror {
		// riporta le osservazioni all'immagine del dispositivo, con le
		// etichette che il modello darebbe guardando quella
		fw := float64(w)
		i468, i473 = [2]float64{fw - i473[0], i473[1]}, [2]float64{fw - i468[0], i468[1]}
	}
	s := CalibSample{Left: l, Right: r, Iris468: i468, Iris473: i473}
	mid := scale(add(l, r), 0.5)
	key := [3]int{int(math.Floor(mid[0] / 25)), int(math.Floor(mid[1] / 25)), int(math.Floor(mid[2] / 25))}
	idx := e.voxels[key]
	switch {
	case len(idx) < 20 && len(e.samples) < 3000:
		if len(idx) == 0 {
			e.newVoxels++
		}
		e.voxels[key] = append(idx, len(e.samples))
		e.samples = append(e.samples, s)
	case len(idx) > 0:
		e.samples[idx[e.rng.Intn(len(idx))]] = s
	}
	e.updateCoverage(mid, len(e.samples), len(e.voxels))
}

// maybeCalibrate avvia una calibrazione in background quando ci sono dati
// nuovi a sufficienza.
func (e *Estimator) maybeCalibrate(w, h int, us int64) {
	if e.cfg.ManualCalibration || e.calibBusy.Load() || len(e.samples) < 120 || len(e.voxels) < 8 {
		return
	}
	interval := int64(5_000_000)
	if e.wcam != nil {
		interval = 60_000_000
		if e.newVoxels < 6 {
			return
		}
	}
	if e.lastCalib != 0 && us-e.lastCalib < interval && us >= e.lastCalib {
		return
	}
	e.lastCalib = us
	e.newVoxels = 0
	snap := append([]CalibSample(nil), e.samples...)
	e.saveSamples(snap)
	e.calibBusy.Store(true)
	go func() {
		cam, q, err := calibrateFast(snap, w, h)
		e.calibDone <- calibOut{cam: cam, q: q, err: err, n: len(snap)}
	}()
}

// saveSamples scrive i campioni della calibrazione nella cartella di debug
// (per analizzarli dopo una prova).
func (e *Estimator) saveSamples(snap []CalibSample) {
	if e.cfg.DebugDir == "" {
		return
	}
	data, err := json.Marshal(snap)
	if err == nil {
		err = os.WriteFile(filepath.Join(e.cfg.DebugDir, "calib-samples.json"), data, 0o644)
	}
	if err != nil {
		e.log.Warn("head pose: calibration samples not saved", "err", err)
	}
}

// calibrateFast stima le inizializzazioni su un sottoinsieme e affina la
// migliore su tutti i campioni.
func calibrateFast(samples []CalibSample, w, h int) (*Camera, CalibQuality, error) {
	sub := samples
	if len(sub) > 300 {
		r := rand.New(rand.NewSource(int64(len(samples))))
		sub = make([]CalibSample, 300)
		for i, j := range r.Perm(len(samples))[:300] {
			sub[i] = samples[j]
		}
	}
	cam, _, err := Calibrate(sub, w, h)
	if err != nil {
		return nil, CalibQuality{}, err
	}
	obs := pairsFor(samples, cam.Mirror, w)
	refine(cam, obs, 40)
	q := quality(cam, obs)
	zmin, zmax := math.Inf(1), math.Inf(-1)
	for _, s := range samples {
		zmin, zmax = math.Min(zmin, math.Min(s.Left[2], s.Right[2])), math.Max(zmax, math.Max(s.Left[2], s.Right[2]))
	}
	q.DepthMM, q.Mirror = zmax-zmin, cam.Mirror
	return cam, q, nil
}

// trackWindowUs: per quanto tempo il ritaglio dell'ultimo frame riuscito resta
// un buon punto di partenza.
const trackWindowUs = 1_500_000

// Soglie per adottare una calibrazione della camera.
// Tarate sul 4C reale: con una raccolta guidata si ottengono 2,9–3,3 px e
// 0,3–0,7°; con queste soglie yaw e roll dell'immagine coincidono con quelli
// degli occhi entro 1–2° (mediana).
const (
	acceptRMSPx    = 4.0
	acceptSigmaDeg = 1.0
)

func (e *Estimator) adoptCalibration(out calibOut) {
	e.calibBusy.Store(false)
	if out.err != nil {
		e.log.Warn("head pose: camera calibration failed", "err", out.err)
		return
	}
	q := out.q
	tilt := cameraTilt(out.cam)
	attrs := []any{"samples", out.n, "focal_px", round1(out.cam.F), "pp", fmt.Sprintf("%.0f,%.0f", out.cam.CX, out.cam.CY),
		"center_mm", fmt.Sprintf("%.1f,%.1f,%.1f", out.cam.C[0], out.cam.C[1], out.cam.C[2]), "tilt_deg", round1(tilt),
		"rms_px", round2(q.RMSPx), "sigma_deg", round2(q.SigmaDeg), "depth_range_mm", math.Round(q.DepthMM), "mirror", q.Mirror}
	if q.RMSPx > acceptRMSPx || q.SigmaDeg > acceptSigmaDeg {
		e.log.Info("head pose: camera calibration not yet reliable (move the head closer/farther and sideways)", attrs...)
		return
	}
	if e.wcam != nil && q.SigmaDeg >= e.wq.SigmaDeg {
		e.log.Debug("head pose: camera calibration not better than the current one", attrs...)
		return
	}
	mirrorChanged := e.wcam == nil || e.wcam.Mirror != out.cam.Mirror
	e.wcam, e.wq = out.cam, q
	e.track = false
	if mirrorChanged && out.cam.Mirror {
		e.log.Warn("head pose: the device image is mirrored; frames will be flipped before the face model")
	}
	e.log.Info("head pose: camera calibrated", attrs...)
	e.mu.Lock()
	serial := e.serial
	e.mu.Unlock()
	if e.cfg.CameraPath != "" {
		if err := out.cam.Save(e.cfg.CameraPath, serial, q); err != nil {
			e.log.Warn("head pose: camera calibration not saved", "err", err)
		} else {
			e.log.Info("head pose: camera calibration saved", "path", e.cfg.CameraPath)
		}
	}
}

// cameraTilt è l'elevazione (gradi) dell'asse ottico nel sistema del tracker.
func cameraTilt(c *Camera) float64 {
	axis := c.R.t().apply(vec3{0, 0, 1})
	return deg(math.Asin(axis[1]))
}

// Stats è un riepilogo per i log periodici.
type Stats struct {
	Frames, Dropped, Processed, Inferences, Searches, Tracked uint64
	MeanProcessMs                                             float64 // per immagine elaborata
	YawDiffDeg, RollDiffDeg                                   float64 // media (immagine − occhi)
	YawRMSDeg, RollRMSDeg                                     float64
	Compared                                                  int
	Last                                                      Result
	Calibrated                                                bool
	Mirror                                                    bool
}

// TakeStats restituisce le statistiche e azzera i confronti con gli occhi.
func (e *Estimator) TakeStats() Stats {
	st := Stats{Frames: e.Frames.Load(), Dropped: e.Dropped.Load(), Processed: e.Processed.Load(),
		Inferences: e.Inferences.Load(), Searches: e.Searches.Load(), Tracked: e.Tracked.Load()}
	if st.Processed > 0 {
		st.MeanProcessMs = float64(e.infNs.Load()) / 1e6 / float64(st.Processed)
	}
	e.mu.Lock()
	d := e.diff
	e.diff = diffStats{}
	st.Last = e.result
	e.mu.Unlock()
	if d.n > 0 {
		n := float64(d.n)
		st.Compared = d.n
		st.YawDiffDeg, st.RollDiffDeg = deg(d.sumYaw/n), deg(d.sumRoll/n)
		st.YawRMSDeg, st.RollRMSDeg = deg(math.Sqrt(d.sqYaw/n)), deg(math.Sqrt(d.sqRoll/n))
	}
	st.Calibrated = st.Last.CameraCalibrated
	return st
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
