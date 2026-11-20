package headpose

import (
	"fmt"
	"math"
	"sort"

	"github.com/ablescia/tobii4c-linux/internal/facemodel"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/nn"
)

// Modello: MediaPipe Face Landmarker, rete "face_landmarks_detector",
// incorporata nel binario (internal/facemodel). Licenza Apache 2.0, © Google LLC.
const (
	ModelInput     = 256 // lato del ritaglio
	NumLandmarks   = 478 // 468 punti della mesh + 10 dell'iride
	modelInputName = "input_12"
	modelLandmarks = "Identity"   // [1,1,1,1434]: x, y, z in pixel del ritaglio
	modelPresence  = "Identity_1" // [1,1,1,1]: logit della presenza del viso
	minFaceScore   = 0.5
	roiScale       = 1.5 // lato del ritaglio / lato del riquadro dei landmark (MediaPipe)
)

// Indici della mesh usati qui. "Destro" e "sinistro" sono quelli del soggetto.
const (
	lmRightEyeOuter = 33
	lmRightEyeInner = 133
	lmLeftEyeOuter  = 263
	lmLeftEyeInner  = 362
	lmNoseTip       = 4
	lmRightIris     = 468 // centro dell'iride, accanto a 33/133
	lmLeftIris      = 473 // centro dell'iride, accanto a 263/362
)

// Landmarks sono i punti del viso nell'immagine: x, y in pixel, z (profondità
// relativa, cresce allontanandosi dalla camera) nella stessa scala di x.
type Landmarks [NumLandmarks][3]float64

// Detector esegue il modello dei landmark su un ritaglio dell'immagine.
type Detector struct {
	model *nn.Model
	samp  *sampler
	lm    []float32
	pres  []float32
}

// NewDetector carica il modello incorporato nel binario (internal/facemodel).
func NewDetector(workers int) (*Detector, error) {
	m, err := nn.Parse(facemodel.ONNX, []int{1, ModelInput, ModelInput, 3})
	if err != nil {
		return nil, fmt.Errorf("face model: %w", err)
	}
	if workers > 0 {
		m.SetWorkers(workers)
	}
	lm, err := m.Output(modelLandmarks)
	if err != nil {
		return nil, err
	}
	pres, err := m.Output(modelPresence)
	if err != nil {
		return nil, err
	}
	if len(lm) != 3*NumLandmarks || len(pres) != 1 {
		return nil, fmt.Errorf("face model: unexpected outputs (%d landmark values, %d scores)", len(lm), len(pres))
	}
	return &Detector{model: m, samp: newSampler(ModelInput), lm: lm, pres: pres}, nil
}

// MACs è il costo di un'inferenza.
func (d *Detector) MACs() int64 { return d.model.MACs() }

// Detect esegue il modello sul ritaglio c e restituisce la probabilità che
// contenga un viso; i landmark sono riportati nelle coordinate dell'immagine.
func (d *Detector) Detect(img *irimg.Frame, c Crop, out *Landmarks) float64 {
	d.samp.sample(img, c, d.model.Input())
	d.model.Run()
	s := c.Side / ModelInput
	for i := range out {
		x, y := c.ToImage(float64(d.lm[3*i]), float64(d.lm[3*i+1]), ModelInput)
		out[i] = [3]float64{x, y, float64(d.lm[3*i+2]) * s}
	}
	return 1 / (1 + math.Exp(-float64(d.pres[0])))
}

// CropFromLandmarks è il ritaglio successivo, come in MediaPipe: ruotato in
// modo che la linea degli occhi sia orizzontale, centrato sul riquadro dei
// landmark e 1,5 volte più grande.
func CropFromLandmarks(lm *Landmarks) Crop {
	r, l := lm[lmRightEyeOuter], lm[lmLeftEyeOuter]
	// Nell'immagine non speculare l'occhio destro del soggetto sta a sinistra:
	// l'asse x del ritaglio va da 33 a 263.
	angle := math.Atan2(l[1]-r[1], l[0]-r[0])
	sin, cos := math.Sincos(angle)
	minU, minV := math.Inf(1), math.Inf(1)
	maxU, maxV := math.Inf(-1), math.Inf(-1)
	for i := 0; i < 468; i++ {
		u := lm[i][0]*cos + lm[i][1]*sin
		v := -lm[i][0]*sin + lm[i][1]*cos
		minU, maxU = math.Min(minU, u), math.Max(maxU, u)
		minV, maxV = math.Min(minV, v), math.Max(maxV, v)
	}
	cu, cv := (minU+maxU)/2, (minV+maxV)/2
	return Crop{
		CX:    cu*cos - cv*sin,
		CY:    cu*sin + cv*cos,
		Side:  roiScale * math.Max(maxU-minU, maxV-minV),
		Angle: angle,
	}
}

// Search cerca un viso nell'immagine senza indicazioni: prova ritagli a tre
// scale su una griglia con passo di mezzo lato, dal centro verso i bordi, e
// si ferma al primo ritaglio molto probabile. Costa fino a una trentina di
// inferenze: serve solo quando il viso è stato perso e la camera non è
// ancora calibrata.
func (d *Detector) Search(img *irimg.Frame, out *Landmarks) (Crop, float64) {
	short := float64(min(img.W, img.H))
	var cands []Crop
	for _, frac := range []float64{0.6, 0.4, 0.85} {
		side := frac * short
		step := side / 2
		nx := int((float64(img.W)-side)/step) + 1
		ny := int((float64(img.H)-side)/step) + 1
		// griglia centrata nell'immagine
		ox := (float64(img.W) - side - float64(nx-1)*step) / 2
		oy := (float64(img.H) - side - float64(ny-1)*step) / 2
		for j := 0; j < ny; j++ {
			for i := 0; i < nx; i++ {
				cands = append(cands, Crop{CX: ox + side/2 + float64(i)*step, CY: oy + side/2 + float64(j)*step, Side: side})
			}
		}
	}
	cx, cy := float64(img.W)/2, float64(img.H)/2
	sort.SliceStable(cands, func(i, j int) bool {
		di := math.Hypot(cands[i].CX-cx, cands[i].CY-cy)
		dj := math.Hypot(cands[j].CX-cx, cands[j].CY-cy)
		return di < dj
	})
	best, bestScore := Crop{}, 0.0
	for _, c := range cands {
		if sc := d.Detect(img, c, out); sc > bestScore {
			best, bestScore = c, sc
			if sc > 0.95 {
				break
			}
		}
	}
	if bestScore < minFaceScore {
		return best, bestScore
	}
	return d.Refine(img, best, out, 2)
}

// Refine riesegue il modello sul ritaglio ricavato dai landmark, iter volte.
func (d *Detector) Refine(img *irimg.Frame, c Crop, out *Landmarks, iter int) (Crop, float64) {
	score := d.Detect(img, c, out)
	for i := 0; i < iter && score >= minFaceScore; i++ {
		c = CropFromLandmarks(out)
		score = d.Detect(img, c, out)
	}
	return c, score
}
