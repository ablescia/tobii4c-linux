package bodypose

import (
	"math"
	"time"
)

// lowPass è LowPassFilter di MediaPipe.
type lowPass struct {
	init       bool
	raw, value float64
}

func (f *lowPass) apply(v, alpha float64) float64 {
	if f.init {
		v = alpha*v + (1-alpha)*f.value
	} else {
		f.init = true
	}
	f.value = v
	return v
}

func (f *lowPass) applyRaw(v, alpha float64) float64 {
	out := f.apply(v, alpha)
	f.raw = v
	return out
}

// oneEuro è OneEuroFilter di MediaPipe (landmarks_smoothing_calculator):
// la frequenza viene dai timestamp, la derivata è scalata da valueScale (1
// diviso la dimensione dell'oggetto) così la soglia non dipende dalla
// distanza.
type oneEuro struct {
	minCutoff, beta, dCutoff float64

	last  time.Time
	freq  float64
	x, dx lowPass
	have  bool
}

func (f *oneEuro) alpha(cutoff float64) float64 {
	te := 1 / f.freq
	tau := 1 / (2 * math.Pi * cutoff)
	return 1 / (1 + tau/te)
}

func (f *oneEuro) apply(t time.Time, valueScale, v float64) float64 {
	if f.have && !t.After(f.last) {
		return v
	}
	if f.have {
		f.freq = 1 / t.Sub(f.last).Seconds()
	} else if f.freq == 0 {
		f.freq = 30
	}
	f.last = t
	d := 0.0
	if f.have {
		d = (v - f.x.raw) * valueScale * f.freq
	}
	f.have = true
	ed := f.dx.apply(d, f.alpha(f.dCutoff))
	cutoff := f.minCutoff + f.beta*math.Abs(ed)
	return f.x.applyRaw(v, f.alpha(cutoff))
}

// pointFilter filtra x, y, z di n punti e la loro visibilità.
type pointFilter struct {
	minCutoff, beta, dCutoff float64
	scaled                   bool // valueScale = 1/scala dell'oggetto
	xyz                      [][3]oneEuro
	vis                      []lowPass
}

func newPointFilter(n int, minCutoff, beta float64, scaled bool) *pointFilter {
	f := &pointFilter{minCutoff: minCutoff, beta: beta, dCutoff: 1, scaled: scaled}
	f.xyz = make([][3]oneEuro, n)
	f.vis = make([]lowPass, n)
	f.reset()
	return f
}

func (f *pointFilter) reset() {
	for i := range f.xyz {
		for k := range f.xyz[i] {
			f.xyz[i][k] = oneEuro{minCutoff: f.minCutoff, beta: f.beta, dCutoff: f.dCutoff}
		}
		f.vis[i] = lowPass{}
	}
}

// visibilityAlpha è il passa-basso di VisibilitySmoothingCalculator.
const visibilityAlpha = 0.1

// apply filtra i punti sul posto; scale è la dimensione dell'oggetto nelle
// unità dei punti (ignorata se il filtro non è scalato).
func (f *pointFilter) apply(t time.Time, scale float64, pts []Landmark) {
	vs := 1.0
	if f.scaled {
		if scale < 1e-6 {
			return
		}
		vs = 1 / scale
	}
	for i := range pts {
		p := &pts[i]
		p.Visibility = f.vis[i].apply(p.Visibility, visibilityAlpha)
		p.X = f.xyz[i][0].apply(t, vs, p.X)
		p.Y = f.xyz[i][1].apply(t, vs, p.Y)
		p.Z = f.xyz[i][2].apply(t, vs, p.Z)
	}
}
