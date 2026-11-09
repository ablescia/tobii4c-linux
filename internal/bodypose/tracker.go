package bodypose

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/tflite"
)

// Landmark è un punto dello scheletro: X, Y in pixel dell'immagine, Z nella
// stessa scala (profondità relativa ai fianchi, negativa verso la camera),
// visibilità e presenza come probabilità.
type Landmark struct {
	X, Y, Z              float64
	Visibility, Presence float64
}

// Pose è il risultato di un frame.
type Pose struct {
	Mode     Mode
	Found    bool    // persona presente (Score ≥ 0,5)
	Score    float64 // presenza stimata dalla rete dei landmark
	N        int     // punti del modello: 33 o 25
	LM       [NumLandmarks]Landmark
	Raw      [NumLandmarks]Landmark   // prima dei filtri
	World    [NumLandmarks][3]float64 // metri, origine tra i fianchi (solo corpo intero)
	HasWorld bool
	Aux      [2]Landmark // punti ausiliari del ritaglio (centro e scala), filtrati

	ROI      ROI  // ritaglio di questo frame
	Tracking bool // ROI dal frame precedente (false: dal detector)
	DetRan   bool // il detector è stato eseguito
	Det      Detection
	DetCount int

	// Mask è la probabilità della persona nel ritaglio ROI (MaskW×MaskH,
	// righe dall'alto): per l'immagine si passa da ROI.FromImage.
	Mask         []float32
	MaskW, MaskH int

	Times Times
}

// Times sono i tempi di un frame.
type Times struct {
	Detect    tflite.Timing // zero se il detector non è stato eseguito
	Landmarks tflite.Timing
	Post      time.Duration // decodifica, filtri, maschera
	Total     time.Duration
}

// Visible dice se il punto i è nell'immagine e non coperto: visibilità e
// presenza almeno 0,5, come nel disegno di MediaPipe (drawing_utils).
func (p *Pose) Visible(i int) bool {
	return p.Found && i < p.N && p.LM[i].Visibility >= 0.5 && p.LM[i].Presence >= 0.5
}

// Tracker esegue la pipeline di MediaPipe Pose su una sequenza di frame.
// Non è concorrente.
type Tracker struct {
	b    tflite.Backend
	Spec Spec

	det, lm tflite.Runner
	dg, lg  *tflite.Graph
	dcfg    detectorConfig
	anchors []anchor

	kpCenter, kpScale int // punti del detector per il ritaglio
	roiScale          float64
	nRaw, keep        int // punti decodificati e punti dello scheletro
	lmIn              int // lato dell'ingresso dei landmark

	oBoxes, oScores                 int
	oLM, oFlag, oSeg, oHeat, oWorld int

	haveROI           bool
	roi               ROI
	fLM, fAux, fWorld *pointFilter
	pose              Pose
	raw               []Landmark
	pts               []normPoint
}

// normPoint è un punto nel ritaglio normalizzato.
type normPoint struct{ x, y, z float64 }

// NewTracker carica i modelli sul backend.
func NewTracker(b tflite.Backend, m *Models) (*Tracker, error) {
	t := &Tracker{b: b, Spec: m.Spec, oSeg: -1, oHeat: -1, oWorld: -1, oLM: -1, oFlag: -1, oBoxes: -1, oScores: -1}
	var err error
	if t.dg, err = tflite.Compile(m.Detector); err != nil {
		return nil, fmt.Errorf("bodypose: detector: %w", err)
	}
	if t.lg, err = tflite.Compile(m.Landmarks); err != nil {
		return nil, fmt.Errorf("bodypose: landmark: %w", err)
	}
	if m.Spec.Mode == UpperBody {
		t.dcfg, t.kpCenter, t.kpScale, t.roiScale = detectorUpper, 2, 3, 1.5
		t.keep = NumUpperLandmarks
	} else {
		t.dcfg, t.kpCenter, t.kpScale, t.roiScale = detectorFull, 0, 1, 1.25
		t.keep = NumLandmarks
	}
	t.nRaw = t.keep + 2
	if is := t.dg.InputShape(); is[1] != t.dcfg.input || is[2] != t.dcfg.input || is[3] != 3 {
		return nil, fmt.Errorf("bodypose: detector con ingresso %v, atteso %d×%d×3", is, t.dcfg.input, t.dcfg.input)
	}
	t.anchors = ssdAnchors(t.dcfg)
	if len(t.anchors) != t.dcfg.anchors {
		return nil, fmt.Errorf("bodypose: %d ancore, attese %d", len(t.anchors), t.dcfg.anchors)
	}
	for k := range t.dg.Outputs {
		s := t.dg.OutputShape(k)
		switch {
		case len(s) == 3 && s[1] == t.dcfg.anchors && s[2] == 12:
			t.oBoxes = k
		case len(s) == 3 && s[1] == t.dcfg.anchors && s[2] == 1:
			t.oScores = k
		}
	}
	if t.oBoxes < 0 || t.oScores < 0 {
		return nil, errors.New("bodypose: uscite del detector non riconosciute")
	}
	is := t.lg.InputShape()
	if is[1] != is[2] || is[3] != 3 {
		return nil, fmt.Errorf("bodypose: rete dei landmark con ingresso %v", is)
	}
	t.lmIn = is[1]
	var flat []int
	for k := range t.lg.Outputs {
		s := t.lg.OutputShape(k)
		n := tflite.Elems(s)
		switch {
		case n == 1:
			t.oFlag = k
		case len(s) == 4 && s[3] == 1:
			t.oSeg = k
		case len(s) == 4:
			t.oHeat = k
		case len(s) == 2:
			flat = append(flat, k)
		}
	}
	for _, k := range flat {
		n := tflite.Elems(t.lg.OutputShape(k))
		if t.oLM < 0 || n > tflite.Elems(t.lg.OutputShape(t.oLM)) {
			t.oLM = k
		}
	}
	if t.oLM < 0 || t.oFlag < 0 || tflite.Elems(t.lg.OutputShape(t.oLM)) < 5*t.nRaw {
		return nil, errors.New("bodypose: uscite dei landmark non riconosciute")
	}
	lmRows := tflite.Elems(t.lg.OutputShape(t.oLM)) / 5
	for _, k := range flat {
		if k != t.oLM && tflite.Elems(t.lg.OutputShape(k)) == 3*lmRows {
			t.oWorld = k
		}
	}
	if t.oHeat >= 0 && t.lg.OutputShape(t.oHeat)[3] < t.nRaw {
		t.oHeat = -1
	}
	if t.det, err = b.Load(t.dg); err != nil {
		return nil, err
	}
	if t.lm, err = b.Load(t.lg); err != nil {
		t.det.Close()
		return nil, err
	}
	// i runner hanno copiato i pesi nella loro memoria: quelli del piano
	// non servono più
	t.dg.ReleaseWeights()
	t.lg.ReleaseWeights()
	t.fLM = newPointFilter(t.keep, 0.05, 80, true)
	t.fAux = newPointFilter(2, 0.01, 10, true)
	t.fWorld = newPointFilter(t.keep, 0.1, 40, false)
	t.raw = make([]Landmark, t.nRaw)
	t.pts = make([]normPoint, t.nRaw)
	return t, nil
}

// Graphs sono i piani di detector e landmark (per la telemetria).
func (t *Tracker) Graphs() (det, lm *tflite.Graph) { return t.dg, t.lg }

// Backend è il backend dei modelli.
func (t *Tracker) Backend() tflite.Backend { return t.b }

// Reset dimentica la persona: il frame successivo passa dal detector.
func (t *Tracker) Reset() {
	t.haveROI = false
	t.fLM.reset()
	t.fAux.reset()
	t.fWorld.reset()
}

// Close libera i modelli (sulla GPU restano fino alla chiusura del backend).
func (t *Tracker) Close() {
	t.det.Close()
	t.lm.Close()
}

// minPresence è la soglia sulla presenza della persona (ThresholdingCalculator).
const minPresence = 0.5

// Process elabora un frame catturato all'istante ts. La Pose restituita è
// riusata dalla chiamata successiva.
func (t *Tracker) Process(img *tflite.Image, ts time.Time) (*Pose, error) {
	t0 := time.Now()
	p := &t.pose
	mask := p.Mask
	*p = Pose{Mode: t.Spec.Mode, N: t.keep, Mask: mask[:0]}
	if err := t.b.SetImage(img); err != nil {
		return nil, err
	}
	roi := t.roi
	p.Tracking = t.haveROI
	if !t.haveROI {
		lb := letterbox(img.W, img.H)
		w := lb.Warp(t.dcfg.input, -1, 1, false)
		if err := t.det.RunImage(&w); err != nil {
			return nil, err
		}
		p.DetRan = true
		p.Times.Detect = t.det.Timing()
		dets := decodeDetections(t.det.Output(t.oBoxes), t.det.Output(t.oScores), t.anchors, t.dcfg.input, lb)
		p.DetCount = len(dets)
		if len(dets) == 0 {
			p.Times.Total = time.Since(t0)
			return p, nil
		}
		p.Det = dets[0]
		c, s := p.Det.Keypoints[t.kpCenter], p.Det.Keypoints[t.kpScale]
		roi = roiFromPoints(c[0], c[1], s[0], s[1], t.roiScale)
	}
	p.ROI = roi
	w := roi.Warp(t.lmIn, 0, 1, true)
	if err := t.lm.RunImage(&w); err != nil {
		return nil, err
	}
	p.Times.Landmarks = t.lm.Timing()
	tp := time.Now()
	p.Score = float64(t.lm.Output(t.oFlag)[0])
	if p.Score < minPresence {
		t.Reset()
		p.Times.Post = time.Since(tp)
		p.Times.Total = time.Since(t0)
		return p, nil
	}
	p.Found = true
	t.decode(p, roi)
	copy(p.Raw[:t.keep], t.raw[:t.keep])
	copy(p.LM[:t.keep], t.raw[:t.keep])
	scale := (roi.W + roi.H) / 2
	t.fLM.apply(ts, scale, p.LM[:t.keep])
	p.Aux = [2]Landmark{t.raw[t.keep], t.raw[t.keep+1]}
	t.fAux.apply(ts, scale, p.Aux[:])
	if p.HasWorld {
		var wl [NumLandmarks]Landmark
		for i := 0; i < t.keep; i++ {
			wl[i] = Landmark{X: p.World[i][0], Y: p.World[i][1], Z: p.World[i][2]}
		}
		t.fWorld.apply(ts, 1, wl[:t.keep])
		for i := 0; i < t.keep; i++ {
			p.World[i] = [3]float64{wl[i].X, wl[i].Y, wl[i].Z}
		}
	}
	t.roi = roiFromPoints(p.Aux[0].X, p.Aux[0].Y, p.Aux[1].X, p.Aux[1].Y, t.roiScale)
	t.haveROI = t.roi.W > 1
	if t.oSeg >= 0 {
		s := t.lg.OutputShape(t.oSeg)
		seg := t.lm.Output(t.oSeg)
		p.MaskW, p.MaskH = s[2], s[1]
		if cap(p.Mask) < len(seg) {
			p.Mask = make([]float32, len(seg))
		}
		p.Mask = p.Mask[:len(seg)]
		for i, v := range seg {
			p.Mask[i] = float32(sigmoid(float64(v)))
		}
	}
	p.Times.Post = time.Since(tp)
	p.Times.Total = time.Since(t0)
	return p, nil
}

// decode legge i punti dal ritaglio (TensorsToLandmarksCalculator), li
// raffina con la heatmap (RefineLandmarksFromHeatmapCalculator) e li porta
// nell'immagine (LandmarkProjectionCalculator); le coordinate 3D vengono
// ruotate come il ritaglio (WorldLandmarkProjectionCalculator).
func (t *Tracker) decode(p *Pose, roi ROI) {
	raw := t.lm.Output(t.oLM)
	n := float64(t.lmIn)
	pts := t.pts
	for i := 0; i < t.nRaw; i++ {
		r := raw[i*5 : i*5+5]
		pts[i] = normPoint{float64(r[0]) / n, float64(r[1]) / n, float64(r[2]) / n}
		t.raw[i].Visibility = sigmoid(float64(r[3]))
		t.raw[i].Presence = sigmoid(float64(r[4]))
	}
	if t.oHeat >= 0 {
		s := t.lg.OutputShape(t.oHeat)
		hm := t.lm.Output(t.oHeat)
		hh, hw, hc := s[1], s[2], s[3]
		const kernel, minConf = 7, 0.5
		off := (kernel - 1) / 2
		for i := range pts {
			cx, cy := int(pts[i].x*float64(hw)), int(pts[i].y*float64(hh))
			if cx < 0 || cx >= hw || cy < 0 || cy >= hh {
				continue
			}
			var sum, wx, wy, top float64
			for row := max(0, cy-off); row < min(hh, cy+off+1); row++ {
				for col := max(0, cx-off); col < min(hw, cx+off+1); col++ {
					c := sigmoid(float64(hm[(row*hw+col)*hc+i]))
					sum += c
					top = math.Max(top, c)
					wx += float64(col) * c
					wy += float64(row) * c
				}
			}
			if top >= minConf && sum > 0 {
				pts[i].x = wx / float64(hw) / sum
				pts[i].y = wy / float64(hh) / sum
			}
		}
	}
	for i, q := range pts {
		t.raw[i].X, t.raw[i].Y = roi.ToImage(q.x, q.y)
		t.raw[i].Z = q.z * roi.W
	}
	if t.oWorld >= 0 {
		wd := t.lm.Output(t.oWorld)
		s, c := math.Sincos(roi.Angle)
		for i := 0; i < t.keep; i++ {
			x, y, z := float64(wd[3*i]), float64(wd[3*i+1]), float64(wd[3*i+2])
			p.World[i] = [3]float64{c*x - s*y, s*x + c*y, z}
		}
		p.HasWorld = true
	}
}
