package headpose

import "math"

// oneEuro è il filtro "1€" (Casiez, Roussel, Vogel 2012): passa-basso con
// frequenza di taglio che cresce con la velocità, per togliere il tremolio
// a testa ferma senza introdurre ritardo nei movimenti rapidi.
type oneEuro struct {
	minCutoff, beta, dCutoff float64
	x, dx                    float64
	lastUs                   int64
	init                     bool
}

func newOneEuro(minCutoff, beta float64) oneEuro {
	return oneEuro{minCutoff: minCutoff, beta: beta, dCutoff: 1}
}

func alpha(cutoff, dt float64) float64 {
	tau := 1 / (2 * math.Pi * cutoff)
	return 1 / (1 + tau/dt)
}

// filter elabora un valore al tempo us (µs del dispositivo).
func (f *oneEuro) filter(v float64, us int64) float64 {
	if !f.init || us <= f.lastUs || us-f.lastUs > 500_000 {
		f.x, f.dx, f.lastUs, f.init = v, 0, us, true
		return v
	}
	dt := float64(us-f.lastUs) / 1e6
	f.lastUs = us
	dx := (v - f.x) / dt
	f.dx += alpha(f.dCutoff, dt) * (dx - f.dx)
	cutoff := f.minCutoff + f.beta*math.Abs(f.dx)
	f.x += alpha(cutoff, dt) * (v - f.x)
	return f.x
}

func (f *oneEuro) reset() { f.init = false }
