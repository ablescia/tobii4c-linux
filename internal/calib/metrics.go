package calib

import (
	"math"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// PointResult è l'esito della verifica su un punto.
type PointResult struct {
	Target      [2]float64   `json:"target"`
	N           int          `json:"n"`             // campioni con sguardo valido
	Total       int          `json:"total"`         // campioni nella finestra
	Mean        [2]float64   `json:"mean"`          // sguardo medio, normalizzato
	ErrorDeg    float64      `json:"error_deg"`     // accuratezza: angolo target–sguardo medio visto dall'occhio
	ErrorMM     float64      `json:"error_mm"`      // stessa distanza sul piano dello schermo
	PrecisionMS float64      `json:"precision_deg"` // precisione: RMS campione-campione (definizione Tobii), sullo sguardo NON filtrato
	DistanceMM  float64      `json:"distance_mm"`   // distanza media occhi–target
	Samples     [][2]float64 `json:"samples"`       // sottocampionati per il disegno
}

func sub3(a, b [3]float64) [3]float64 { return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func norm3(a [3]float64) float64      { return math.Sqrt(a[0]*a[0] + a[1]*a[1] + a[2]*a[2]) }

func angleDeg(a, b [3]float64) float64 {
	na, nb := norm3(a), norm3(b)
	if na == 0 || nb == 0 {
		return math.NaN()
	}
	c := (a[0]*b[0] + a[1]*b[1] + a[2]*b[2]) / (na * nb)
	return math.Acos(math.Max(-1, math.Min(1, c))) * 180 / math.Pi
}

// Evaluate calcola accuratezza e precisione di una finestra di campioni
// rispetto a un target, usando la display area per passare in mm e la
// posizione degli occhi (sistema del tracker) come vertice degli angoli.
func Evaluate(target [2]float64, samples []gaze.Sample, area ttp.DisplayArea) PointResult {
	res := PointResult{Target: target, Total: len(samples)}
	tgt := area.PointMM(target[0], target[1])
	var eye [3]float64
	var nEye int
	var prev [3]float64
	var sumSq float64
	var nPairs int
	var sx, sy float64
	for i := range samples {
		s := &samples[i]
		if !s.GazeValid {
			continue
		}
		res.N++
		sx += float64(s.Gaze[0])
		sy += float64(s.Gaze[1])
		// Vertice degli angoli nel sistema del tracker, lo stesso di PointMM.
		if mid, ok := s.EyesTrackerMM(); ok {
			for k := 0; k < 3; k++ {
				eye[k] += float64(mid[k])
			}
			nEye++
		}
	}
	if res.N == 0 {
		return res
	}
	res.Mean = [2]float64{sx / float64(res.N), sy / float64(res.N)}
	if nEye == 0 {
		// Nessuna posizione degli occhi: distanza nominale di 600 mm davanti al centro.
		c := area.PointMM(0.5, 0.5)
		eye = [3]float64{c[0], c[1], c[2] + 600}
	} else {
		for k := range eye {
			eye[k] /= float64(nEye)
		}
	}
	meanMM := area.PointMM(res.Mean[0], res.Mean[1])
	if res.ErrorDeg = angleDeg(sub3(tgt, eye), sub3(meanMM, eye)); math.IsNaN(res.ErrorDeg) {
		res.ErrorDeg = 0
	}
	res.ErrorMM = norm3(sub3(tgt, meanMM))
	res.DistanceMM = norm3(sub3(tgt, eye))

	// Precisione RMS-S2S: radice della media dei quadrati degli angoli tra
	// campioni consecutivi validi. Va misurata sullo sguardo non filtrato
	// ("Remote Gaze Point Responsive Combined", 0x20): quello filtrato (0x1c)
	// è levigato dal firmware e sul 4C reale dà ~0,01°, un valore che non
	// descrive il rumore del tracker. Anche i punti disegnati sono quelli grezzi.
	have := false
	step := max(1, res.N/60)
	k := 0
	for i := range samples {
		s := &samples[i]
		g, valid := s.GazeRaw, s.GazeRawValid
		if !valid {
			g, valid = s.Gaze, s.GazeValid
		}
		if !valid {
			have = false
			continue
		}
		p := area.PointMM(float64(g[0]), float64(g[1]))
		if have {
			a := angleDeg(sub3(prev, eye), sub3(p, eye))
			sumSq += a * a
			nPairs++
		}
		prev, have = p, true
		if k%step == 0 {
			res.Samples = append(res.Samples, [2]float64{float64(g[0]), float64(g[1])})
		}
		k++
	}
	if nPairs > 0 {
		res.PrecisionMS = math.Sqrt(sumSq / float64(nPairs))
	}
	return res
}

// gazeDistanceMM: distanza sul piano dello schermo tra due punti normalizzati.
func gazeDistanceMM(a, b [2]float64, area ttp.DisplayArea) float64 {
	return norm3(sub3(area.PointMM(a[0], a[1]), area.PointMM(b[0], b[1])))
}
