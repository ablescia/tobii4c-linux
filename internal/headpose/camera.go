package headpose

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"
)

// Camera è il modello pinhole della camera del tracker: proietta un punto nel
// sistema del tracker (mm) in pixel dell'immagine 0x501. Il firmware non
// espone i parametri della camera: il driver li stima confrontando i centri
// dell'iride trovati nell'immagine con i centri corneali 3D del tracker.
type Camera struct {
	F      float64 // focale, pixel
	CX, CY float64 // punto principale, pixel
	R      mat3    // rotazione tracker → camera (camera: x a destra, y in basso, z in avanti)
	C      vec3    // centro della camera nel sistema del tracker, mm
	Mirror bool    // l'immagine del dispositivo è speculare: va ribaltata prima del modello
	W, H   int
}

// toCamera porta un punto del tracker nel sistema della camera.
func (c *Camera) toCamera(p vec3) vec3 { return c.R.apply(sub(p, c.C)) }

// Project proietta un punto del tracker; ok è false se sta dietro la camera.
func (c *Camera) Project(p vec3) (u, v float64, ok bool) {
	q := c.toCamera(p)
	if q[2] <= 1 {
		return 0, 0, false
	}
	return c.F*q[0]/q[2] + c.CX, c.F*q[1]/q[2] + c.CY, true
}

// CameraFile è la forma su disco della calibrazione della camera.
type CameraFile struct {
	Serial   string     `json:"serial"`
	Updated  time.Time  `json:"updated"`
	F        float64    `json:"focal_px"`
	CX       float64    `json:"cx_px"`
	CY       float64    `json:"cy_px"`
	RotVec   [3]float64 `json:"rotation_tracker_to_camera"`
	CenterMM [3]float64 `json:"center_mm"`
	Mirror   bool       `json:"mirror"`
	W        int        `json:"width"`
	H        int        `json:"height"`
	RMSPx    float64    `json:"rms_px"`
	SigmaDeg float64    `json:"rotation_sigma_deg"`
	Pairs    int        `json:"pairs"`
	DepthMM  float64    `json:"depth_range_mm"`
}

// Save scrive la calibrazione in path (JSON).
func (c *Camera) Save(path, serial string, q CalibQuality) error {
	f := CameraFile{Serial: serial, Updated: time.Now().UTC().Truncate(time.Second),
		F: c.F, CX: c.CX, CY: c.CY, RotVec: rotvec(c.R), CenterMM: c.C, Mirror: c.Mirror,
		W: c.W, H: c.H, RMSPx: q.RMSPx, SigmaDeg: q.SigmaDeg, Pairs: q.Pairs, DepthMM: q.DepthMM}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadCamera legge una calibrazione salvata. Se serial non è vuoto deve
// coincidere con quello del file.
func LoadCamera(path, serial string) (*Camera, CameraFile, error) {
	var f CameraFile
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, f, err
	}
	if serial != "" && f.Serial != serial {
		return nil, f, fmt.Errorf("camera calibration is for %s, device is %s", f.Serial, serial)
	}
	if f.F <= 0 || f.W <= 0 || f.H <= 0 {
		return nil, f, errors.New("invalid camera calibration")
	}
	return &Camera{F: f.F, CX: f.CX, CY: f.CY, R: rodrigues(f.RotVec), C: f.CenterMM,
		Mirror: f.Mirror, W: f.W, H: f.H}, f, nil
}

// CalibSample è un'osservazione per la calibrazione: i centri corneali dei
// due occhi (sistema del tracker, mm, "sinistro"/"destro" dell'utente come
// nel tracker) e i centri delle due iridi nell'immagine così come arriva dal
// dispositivo, con le etichette che il modello del viso dà guardando
// quell'immagine (468 accanto agli angoli 33/133, 473 accanto a 263/362).
type CalibSample struct {
	Left, Right vec3
	Iris468     [2]float64
	Iris473     [2]float64
}

// pair è una corrispondenza punto 3D ↔ pixel per il risolutore.
type pair struct {
	P    vec3
	U, V float64
}

// pairs costruisce le corrispondenze per un'ipotesi. Immagine diritta:
// l'occhio destro del soggetto (468) è il destro dell'utente. Immagine
// speculare: dopo il ribaltamento orizzontale le etichette del modello si
// scambiano.
func pairsFor(samples []CalibSample, mirror bool, w int) []pair {
	out := make([]pair, 0, 2*len(samples))
	for _, s := range samples {
		if !mirror {
			out = append(out, pair{s.Right, s.Iris468[0], s.Iris468[1]}, pair{s.Left, s.Iris473[0], s.Iris473[1]})
			continue
		}
		fw := float64(w)
		out = append(out, pair{s.Right, fw - s.Iris473[0], s.Iris473[1]}, pair{s.Left, fw - s.Iris468[0], s.Iris468[1]})
	}
	return out
}

// CalibQuality riassume una calibrazione.
type CalibQuality struct {
	RMSPx    float64 // errore di riproiezione
	SigmaDeg float64 // incertezza (1σ) della rotazione, gradi
	Pairs    int
	DepthMM  float64 // escursione in profondità dei punti usati
	Mirror   bool
}

// Prior deboli: la camera sta dentro la barra del tracker (sottile in altezza
// e profondità, ma larga: sul 4C provato la camera risulta a ~5 cm dal centro)
// e il punto principale vicino al centro dell'immagine.
var sigmaCenter = vec3{60, 15, 15} // mm, per asse del tracker

const (
	sigmaPx = 1.5 // rumore atteso del centro dell'iride
	huberPx = 3.0
)

// sigmaPPVar: prior sul punto principale (px). Stretto di proposito: i dati
// raccolti davanti al tracker coprono una parte piccola dell'immagine, e uno
// spostamento del punto principale vi equivale quasi esattamente a una
// rotazione della camera (sul 4C reale, con 30 px di tolleranza, il
// risolutore lo spostava di 260 px ruotando la camera di 20°). La scelta però
// conta: la profondità stimata dal modello del viso è riferita al vero asse
// ottico, che in una camera normale passa vicino al centro del sensore.
var sigmaPPVar = 5.0

// Calibrate stima la camera dai campioni. Prova l'immagine diritta e quella
// speculare, ciascuna con più inizializzazioni di focale e inclinazione, e
// tiene la soluzione con il costo minore.
func Calibrate(samples []CalibSample, w, h int) (*Camera, CalibQuality, error) {
	if len(samples) < 30 {
		return nil, CalibQuality{}, fmt.Errorf("only %d samples", len(samples))
	}
	zmin, zmax := math.Inf(1), math.Inf(-1)
	for _, s := range samples {
		for _, p := range []vec3{s.Left, s.Right} {
			zmin, zmax = math.Min(zmin, p[2]), math.Max(zmax, p[2])
		}
	}
	var best *Camera
	var bestObs []pair
	bestCost := math.Inf(1)
	for _, mirror := range []bool{false, true} {
		obs := pairsFor(samples, mirror, w)
		for _, f0 := range []float64{400, 600, 850, 1200} {
			for _, tilt := range []float64{-30, -15, 0, 15, 30} {
				// camera che guarda l'utente: x_cam = −X, y_cam = −Y, z_cam = +Z,
				// poi inclinata attorno al proprio asse x
				R0 := rodrigues(vec3{tilt * math.Pi / 180, 0, 0}).mul(mat3{{-1, 0, 0}, {0, -1, 0}, {0, 0, 1}})
				cam := &Camera{F: f0, CX: float64(w) / 2, CY: float64(h) / 2, R: R0, Mirror: mirror, W: w, H: h}
				cost := refine(cam, obs, 60)
				if cost < bestCost {
					best, bestObs, bestCost = cam, obs, cost
				}
			}
		}
	}
	q := quality(best, bestObs)
	q.DepthMM = zmax - zmin
	q.Mirror = best.Mirror
	return best, q, nil
}

const nParams = 9 // δrot(3), C(3), f, cx, cy

func perturbed(c *Camera, d [nParams]float64) *Camera {
	n := *c
	n.R = rodrigues(vec3{d[0], d[1], d[2]}).mul(c.R)
	n.C = add(c.C, vec3{d[3], d[4], d[5]})
	n.F += d[6]
	n.CX += d[7]
	n.CY += d[8]
	return &n
}

// residuals restituisce i residui pesati (pixel / sigma, più i prior) e i
// pesi di Huber dei residui di pixel.
func residuals(c *Camera, obs []pair, r []float64) {
	for i, o := range obs {
		u, v, ok := c.Project(o.P)
		if !ok {
			u, v = o.U+1e3, o.V+1e3
		}
		r[2*i], r[2*i+1] = (u-o.U)/sigmaPx, (v-o.V)/sigmaPx
	}
	k := 2 * len(obs)
	r[k], r[k+1], r[k+2] = c.C[0]/sigmaCenter[0], c.C[1]/sigmaCenter[1], c.C[2]/sigmaCenter[2]
	r[k+3], r[k+4] = (c.CX-float64(c.W)/2)/sigmaPPVar, (c.CY-float64(c.H)/2)/sigmaPPVar
}

func huberWeight(r float64) float64 {
	const t = huberPx / sigmaPx
	if a := math.Abs(r); a > t {
		return t / a
	}
	return 1
}

// refine è un Levenberg-Marquardt con jacobiano numerico e pesi di Huber
// (IRLS). Modifica c e restituisce il costo finale.
func refine(c *Camera, obs []pair, iters int) float64 {
	m := 2*len(obs) + 5
	r := make([]float64, m)
	rp := make([]float64, m)
	J := make([][nParams]float64, m)
	steps := [nParams]float64{1e-5, 1e-5, 1e-5, 1e-3, 1e-3, 1e-3, 1e-2, 1e-2, 1e-2}
	cost := func(cam *Camera) float64 {
		residuals(cam, obs, rp)
		s := 0.0
		for i, v := range rp {
			w := 1.0
			if i < 2*len(obs) {
				w = huberWeight(v)
			}
			s += w * v * v
		}
		return s
	}
	lambda := 1e-3
	cur := cost(c)
	for it := 0; it < iters; it++ {
		residuals(c, obs, r)
		for j := 0; j < nParams; j++ {
			var d [nParams]float64
			d[j] = steps[j]
			residuals(perturbed(c, d), obs, rp)
			for i := range r {
				J[i][j] = (rp[i] - r[i]) / steps[j]
			}
		}
		var A [nParams][nParams]float64
		var g [nParams]float64
		for i := range r {
			w := 1.0
			if i < 2*len(obs) {
				w = huberWeight(r[i])
			}
			for a := 0; a < nParams; a++ {
				g[a] += w * J[i][a] * r[i]
				for b := a; b < nParams; b++ {
					A[a][b] += w * J[i][a] * J[i][b]
				}
			}
		}
		for a := 0; a < nParams; a++ {
			for b := 0; b < a; b++ {
				A[a][b] = A[b][a]
			}
		}
		improved := false
		for try := 0; try < 10; try++ {
			M := A
			for a := 0; a < nParams; a++ {
				M[a][a] += lambda * math.Max(A[a][a], 1e-9)
			}
			var rhs [nParams]float64
			for a := range rhs {
				rhs[a] = -g[a]
			}
			delta, ok := solve9(M, rhs)
			if !ok {
				lambda *= 10
				continue
			}
			nc := perturbed(c, delta)
			if nc.F < 50 {
				lambda *= 10
				continue
			}
			if nk := cost(nc); nk < cur {
				*c, cur = *nc, nk
				lambda = math.Max(lambda/3, 1e-9)
				improved = true
				break
			}
			lambda *= 10
		}
		if !improved {
			break
		}
	}
	return cur
}

// quality calcola RMS di riproiezione e incertezza della rotazione.
func quality(c *Camera, obs []pair) CalibQuality {
	q := CalibQuality{Pairs: len(obs)}
	m := 2*len(obs) + 5
	r := make([]float64, m)
	rp := make([]float64, m)
	residuals(c, obs, r)
	s := 0.0
	for i := 0; i < 2*len(obs); i++ {
		s += r[i] * r[i]
	}
	q.RMSPx = math.Sqrt(s/float64(len(obs))) * sigmaPx // per punto (u e v)
	// covarianza ≈ (JᵀJ)⁻¹ con i residui in unità di sigma
	steps := [nParams]float64{1e-5, 1e-5, 1e-5, 1e-3, 1e-3, 1e-3, 1e-2, 1e-2, 1e-2}
	var A [nParams][nParams]float64
	J := make([][nParams]float64, m)
	for j := 0; j < nParams; j++ {
		var d [nParams]float64
		d[j] = steps[j]
		residuals(perturbed(c, d), obs, rp)
		for i := range r {
			J[i][j] = (rp[i] - r[i]) / steps[j]
		}
	}
	for i := range r {
		for a := 0; a < nParams; a++ {
			for b := 0; b < nParams; b++ {
				A[a][b] += J[i][a] * J[i][b]
			}
		}
	}
	// varianza della rotazione: blocco 3×3 dell'inversa
	var worst float64
	for a := 0; a < 3; a++ {
		var e [nParams]float64
		e[a] = 1
		col, ok := solve9(A, e)
		if !ok {
			return q
		}
		worst = math.Max(worst, col[a])
	}
	// i residui sono normalizzati con sigmaPx: si riscala con l'errore reale
	scaleF := math.Max(q.RMSPx/sigmaPx, 1)
	q.SigmaDeg = math.Sqrt(worst) * scaleF * 180 / math.Pi
	return q
}

// solve9 risolve A·x = b (eliminazione di Gauss con pivot parziale).
func solve9(A [nParams][nParams]float64, b [nParams]float64) ([nParams]float64, bool) {
	var x [nParams]float64
	for col := 0; col < nParams; col++ {
		p := col
		for r := col + 1; r < nParams; r++ {
			if math.Abs(A[r][col]) > math.Abs(A[p][col]) {
				p = r
			}
		}
		if math.Abs(A[p][col]) < 1e-15 {
			return x, false
		}
		A[col], A[p] = A[p], A[col]
		b[col], b[p] = b[p], b[col]
		for r := col + 1; r < nParams; r++ {
			f := A[r][col] / A[col][col]
			for k := col; k < nParams; k++ {
				A[r][k] -= f * A[col][k]
			}
			b[r] -= f * b[col]
		}
	}
	for r := nParams - 1; r >= 0; r-- {
		s := b[r]
		for k := r + 1; k < nParams; k++ {
			s -= A[r][k] * x[k]
		}
		x[r] = s / A[r][r]
	}
	return x, true
}
