package bodypose

import "math"

// Joint è un angolo articolare (gradi; OK falso se i tre punti non sono
// tutti visibili).
type Joint struct {
	Deg float64
	OK  bool
}

// Articolazioni misurate, sinistra e destra del soggetto.
const (
	JShoulderL = iota // tra braccio e fianco (spalla–gomito, spalla–anca)
	JShoulderR
	JElbowL // spalla–gomito–polso
	JElbowR
	JHipL // spalla–anca–ginocchio
	JHipR
	JKneeL // anca–ginocchio–caviglia
	JKneeR
	NumJoints
)

// JointNames sono i nomi delle articolazioni.
var JointNames = [NumJoints]string{"spalla sx", "spalla dx", "gomito sx", "gomito dx", "anca sx", "anca dx",
	"ginocchio sx", "ginocchio dx"}

var jointPoints = [NumJoints][3]int{
	{LeftElbow, LeftShoulder, LeftHip}, {RightElbow, RightShoulder, RightHip},
	{LeftShoulder, LeftElbow, LeftWrist}, {RightShoulder, RightElbow, RightWrist},
	{LeftShoulder, LeftHip, LeftKnee}, {RightShoulder, RightHip, RightKnee},
	{LeftHip, LeftKnee, LeftAnkle}, {RightHip, RightKnee, RightAnkle},
}

// Posture sono le posture riconosciute.
const (
	PostureUnknown  = iota // punti insufficienti
	PostureStanding        // in piedi
	PostureSitting         // seduto
	PostureCrouch          // accovacciato
	PostureLying           // sdraiato
	PostureUpper           // mezzo busto: solo la parte superiore
)

// PostureNames sono i nomi delle posture.
var PostureNames = []string{"non determinabile", "in piedi", "seduto", "accovacciato", "sdraiato", "busto (seduto)"}

// Metrics sono le misure ricavate da una posa.
type Metrics struct {
	Joints   [NumJoints]Joint
	Joints3D bool // angoli dalle coordinate 3D (altrimenti nel piano dell'immagine)

	Posture  int
	ThighDeg [2]float64 // cosce dalla verticale (3D), sx e dx; NaN se non visibili

	// Busto: inclinazione laterale nell'immagine (positiva verso la destra
	// dell'immagine), in avanti verso la camera (3D) e rotazione attorno
	// alla verticale (3D, positiva con la spalla destra del soggetto più
	// lontana).
	LeanDeg, ForwardDeg, YawDeg float64
	LeanOK, ForwardOK, YawOK    bool

	ShoulderTiltDeg float64 // linea delle spalle dall'orizzontale (positiva: spalla dx del soggetto più in basso nell'immagine)
	ShoulderPx      float64 // larghezza delle spalle nell'immagine
	ShouldersOK     bool
	HeadTiltDeg     float64 // linea degli occhi dall'orizzontale (positiva: testa piegata verso la spalla dx)
	HeadOK          bool
	HandUp          [2]bool // polso sopra la spalla, sx e dx

	DistM      float64 // distanza stimata dalla camera
	DistAssume bool    // con una larghezza delle spalle ipotizzata (niente 3D)
	DistOK     bool

	Visible   int     // punti visibili
	MeanVis   float64 // visibilità media dei punti del modello
	BoxPx     [4]float64
	HeightPx  float64 // altezza del riquadro dei punti visibili
	CenterRel [2]float64
}

// ShoulderWidthM è la larghezza delle spalle (biacromiale) ipotizzata
// quando mancano le coordinate 3D: media degli adulti.
const ShoulderWidthM = 0.39

func sub3(a, b [3]float64) [3]float64 { return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func dot3(a, b [3]float64) float64    { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func norm3(a [3]float64) float64      { return math.Sqrt(dot3(a, a)) }

// angleAt è l'angolo in b del triangolo a-b-c, in gradi.
func angleAt(a, b, c [3]float64) float64 {
	u, v := sub3(a, b), sub3(c, b)
	nu, nv := norm3(u), norm3(v)
	if nu == 0 || nv == 0 {
		return math.NaN()
	}
	return math.Acos(math.Max(-1, math.Min(1, dot3(u, v)/(nu*nv)))) * 180 / math.Pi
}

// fromVertical è l'angolo di v dalla verticale (asse y), in gradi.
func fromVertical(v [3]float64) float64 {
	n := norm3(v)
	if n == 0 {
		return math.NaN()
	}
	return math.Acos(math.Min(1, math.Abs(v[1])/n)) * 180 / math.Pi
}

func mid3(a, b [3]float64) [3]float64 {
	return [3]float64{(a[0] + b[0]) / 2, (a[1] + b[1]) / 2, (a[2] + b[2]) / 2}
}

// Measure calcola le misure di p; focal è la focale della camera in pixel
// (0: niente distanza).
func Measure(p *Pose, focal float64) Metrics {
	var m Metrics
	m.ThighDeg = [2]float64{math.NaN(), math.NaN()}
	if !p.Found {
		return m
	}
	vis := p.Visible
	img := func(i int) [3]float64 { return [3]float64{p.LM[i].X, p.LM[i].Y, 0} }
	pt := img
	if p.HasWorld {
		pt = func(i int) [3]float64 { return p.World[i] }
		m.Joints3D = true
	}
	for j, q := range jointPoints {
		if q[0] < p.N && q[2] < p.N && vis(q[0]) && vis(q[1]) && vis(q[2]) {
			d := angleAt(pt(q[0]), pt(q[1]), pt(q[2]))
			m.Joints[j] = Joint{Deg: d, OK: !math.IsNaN(d)}
		}
	}

	// riquadro e visibilità
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for i := 0; i < p.N; i++ {
		m.MeanVis += p.LM[i].Visibility
		if !vis(i) {
			continue
		}
		m.Visible++
		minX, maxX = math.Min(minX, p.LM[i].X), math.Max(maxX, p.LM[i].X)
		minY, maxY = math.Min(minY, p.LM[i].Y), math.Max(maxY, p.LM[i].Y)
	}
	m.MeanVis /= float64(p.N)
	if m.Visible > 0 {
		m.BoxPx = [4]float64{minX, minY, maxX, maxY}
		m.HeightPx = maxY - minY
	}

	ls, rs := LeftShoulder, RightShoulder
	if vis(ls) && vis(rs) {
		dx, dy := p.LM[rs].X-p.LM[ls].X, p.LM[rs].Y-p.LM[ls].Y
		// la spalla sinistra del soggetto sta di norma a destra nell'immagine
		m.ShoulderTiltDeg = math.Atan2(dy, -dx) * 180 / math.Pi
		m.ShoulderPx = math.Hypot(dx, dy)
		m.ShouldersOK = true
	}
	if vis(LeftEye) && vis(RightEye) {
		dx, dy := p.LM[RightEye].X-p.LM[LeftEye].X, p.LM[RightEye].Y-p.LM[LeftEye].Y
		m.HeadTiltDeg = math.Atan2(dy, -dx) * 180 / math.Pi
		m.HeadOK = true
	}
	for k, w := range [2]int{LeftWrist, RightWrist} {
		s := [2]int{LeftShoulder, RightShoulder}[k]
		m.HandUp[k] = vis(w) && vis(s) && p.LM[w].Y < p.LM[s].Y
	}

	// busto: dal centro dei fianchi al centro delle spalle
	hipsOK := vis(LeftHip) && vis(RightHip)
	if m.ShouldersOK && hipsOK {
		sm := mid3(img(ls), img(rs))
		hm := mid3(img(LeftHip), img(RightHip))
		v := sub3(sm, hm)
		m.LeanDeg = math.Atan2(v[0], -v[1]) * 180 / math.Pi
		m.LeanOK = true
		if p.HasWorld {
			ws := mid3(p.World[ls], p.World[rs])
			wh := mid3(p.World[LeftHip], p.World[RightHip])
			t := sub3(ws, wh)
			// z cresce allontanandosi dalla camera: in avanti è z negativa
			m.ForwardDeg = math.Atan2(-t[2], -t[1]) * 180 / math.Pi
			m.ForwardOK = true
		}
	}
	if p.HasWorld && m.ShouldersOK {
		d := sub3(p.World[rs], p.World[ls])
		// di fronte alla camera la spalla dx del soggetto ha x minore
		m.YawDeg = math.Atan2(d[2], -d[0]) * 180 / math.Pi
		m.YawOK = true
	}

	// distanza
	if focal > 0 && m.ShouldersOK && m.ShoulderPx > 4 {
		if p.HasWorld {
			// lunghezza proiettata nel piano dell'immagine dei segmenti del
			// busto, nel modello (metri) e nell'immagine (pixel)
			var zs, ws float64
			seg := [][2]int{{ls, rs}, {LeftHip, RightHip}, {ls, LeftHip}, {rs, RightHip}}
			for _, s := range seg {
				if !vis(s[0]) || !vis(s[1]) {
					continue
				}
				a, b := p.World[s[0]], p.World[s[1]]
				lw := math.Hypot(a[0]-b[0], a[1]-b[1])
				lp := math.Hypot(p.LM[s[0]].X-p.LM[s[1]].X, p.LM[s[0]].Y-p.LM[s[1]].Y)
				if lp > 4 && lw > 0.05 {
					// media di focal·lw/lp pesata dalla lunghezza in pixel
					zs += focal * lw
					ws += lp
				}
			}
			if ws > 0 {
				m.DistM, m.DistOK = zs/ws, true
			}
		} else {
			m.DistM, m.DistOK, m.DistAssume = focal*ShoulderWidthM/m.ShoulderPx, true, true
		}
	}

	m.Posture = classify(p, &m)
	return m
}

// classify riconosce la postura dalle coordinate 3D di busto e gambe.
func classify(p *Pose, m *Metrics) int {
	if p.Mode == UpperBody {
		return PostureUpper
	}
	if !p.HasWorld || !m.ShouldersOK {
		return PostureUnknown
	}
	vis := p.Visible
	if !vis(LeftHip) || !vis(RightHip) {
		return PostureUnknown
	}
	w := p.World
	trunk := sub3(mid3(w[LeftShoulder], w[RightShoulder]), mid3(w[LeftHip], w[RightHip]))
	if fromVertical(trunk) > 60 {
		return PostureLying
	}
	n := 0
	var thigh, knee float64
	kneeN := 0
	for k, h := range [2]int{LeftHip, RightHip} {
		kn := [2]int{LeftKnee, RightKnee}[k]
		if !vis(kn) {
			continue
		}
		a := fromVertical(sub3(w[kn], w[h]))
		m.ThighDeg[k] = a
		thigh += a
		n++
		if j := m.Joints[JKneeL+k]; j.OK {
			knee += j.Deg
			kneeN++
		}
	}
	if n == 0 {
		return PostureUnknown
	}
	thigh /= float64(n)
	if kneeN > 0 {
		knee /= float64(kneeN)
	} else {
		knee = math.NaN()
	}
	switch {
	case thigh < 30 && (math.IsNaN(knee) || knee > 140):
		return PostureStanding
	case thigh > 50 && (math.IsNaN(knee) || knee > 60):
		if !math.IsNaN(knee) && knee < 75 && thigh < 70 {
			return PostureCrouch
		}
		return PostureSitting
	case !math.IsNaN(knee) && knee < 100:
		return PostureCrouch
	}
	return PostureUnknown
}
