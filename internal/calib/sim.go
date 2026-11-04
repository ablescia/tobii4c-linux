package calib

import (
	"context"
	"crypto/rand"
	"math"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// SimDevice simula un 4C per provare l'interfaccia senza hardware: occhi a
// ~62 cm che ondeggiano, sguardo che insegue il punto mostrato con latenza
// saccadica, rumore, un errore sistematico che il "compute" riduce, e
// battiti di ciglia periodici. Geometria di montaggio = quella reale del 4C.
type SimDevice struct {
	hub *Hub
	// TimeScale accorcia i tempi simulati (1 = come l'hardware; i test usano ~0,02).
	TimeScale float64
	// OnNotify riceve le notifiche simulate (3220 dopo ogni compute).
	OnNotify func(id uint32, payload []byte)
	calID    uint32

	mu      sync.Mutex
	area    ttp.DisplayArea
	mount   ttp.Mounting
	blob    []byte
	target  [2]float64
	visible bool
	shownAt time.Time
	gaze    [2]float64
	bias    [2]float64
	// uncalibrated: dopo il clear e fino al primo compute il 4C reale non
	// fornisce sguardo (osservato: il primo punto non può essere verificato).
	uncalibrated bool
}

// NewSimDevice crea il simulatore che scrive campioni in hub.
func NewSimDevice(hub *Hub) *SimDevice {
	m := ttp.Mounting{Guides: 2, WidthMM: 184, AngleDeg: 20,
		ExternalMM: [3]float64{0, -4, 12}, InternalMM: [3]float64{0, 9, 8}}
	blob := make([]byte, 335880)
	_, _ = rand.Read(blob)
	return &SimDevice{hub: hub, TimeScale: 1, mount: m, area: ttp.DisplayAreaBasic(652.2, 323.6, -0.1, m),
		blob: blob, gaze: [2]float64{0.5, 0.5}, bias: [2]float64{0.035, -0.025}}
}

// Identity imita le stringhe del 4C.
func (d *SimDevice) Identity() []string {
	return []string{"SIMULATED-0000", "IS4_Large_Peripheral (simulato)", "IS4", "4.7.0-sim"}
}

// DisplayInfo restituisce area e mounting.
func (d *SimDevice) DisplayInfo() (*ttp.DisplayArea, *ttp.Mounting) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, m := d.area, d.mount
	return &a, &m
}

func (d *SimDevice) sleep(t time.Duration) { time.Sleep(time.Duration(float64(t) * d.TimeScale)) }

// ReadCalibration restituisce il blob simulato.
func (d *SimDevice) ReadCalibration() ([]byte, error) {
	d.sleep(150 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.blob...), nil
}

// WriteCalibration sostituisce il blob simulato (e riporta l'errore iniziale).
func (d *SimDevice) WriteCalibration(b []byte) error {
	d.sleep(300 * time.Millisecond)
	d.mu.Lock()
	d.blob = append([]byte(nil), b...)
	d.bias = [2]float64{0.035, -0.025}
	d.uncalibrated = false
	d.mu.Unlock()
	return nil
}

// WriteDisplayArea memorizza l'area.
func (d *SimDevice) WriteDisplayArea(a ttp.DisplayArea) error {
	d.mu.Lock()
	d.area = a
	d.mu.Unlock()
	return nil
}

// Command simula i tempi misurati sull'hardware.
func (d *SimDevice) Command(op uint32, _ []byte, _ time.Duration) error {
	switch op {
	case ttp.OpCalCollect2D:
		d.sleep(700 * time.Millisecond)
	case ttp.OpCalCompute:
		d.sleep(900 * time.Millisecond)
		d.mu.Lock()
		d.calID++
		id := d.calID
		if d.OnNotify != nil {
			defer d.OnNotify(ttp.NotifyCalibrationID, ttp.AppendU32(ttp.PayloadPrefix(nil), id))
		}
		// Ogni calcolo riduce l'errore sistematico, fino a un residuo di ~0,4°.
		d.uncalibrated = false
		d.bias[0] = 0.006 + (d.bias[0]-0.006)*0.25
		d.bias[1] = -0.004 + (d.bias[1]+0.004)*0.25
		d.mu.Unlock()
	case ttp.OpCalClear:
		d.sleep(20 * time.Millisecond)
		d.mu.Lock()
		d.uncalibrated = true
		d.mu.Unlock()
	default:
		d.sleep(20 * time.Millisecond)
	}
	return nil
}

// SetStimulus riceve la posizione del punto mostrato.
func (d *SimDevice) SetStimulus(x, y float64, visible bool) {
	d.mu.Lock()
	if visible && (!d.visible || d.target != [2]float64{x, y}) {
		d.shownAt = time.Now()
	}
	d.target, d.visible = [2]float64{x, y}, visible
	d.mu.Unlock()
}

// Run genera campioni a 90 Hz finché ctx non viene cancellato.
func (d *SimDevice) Run(ctx context.Context) {
	tk := time.NewTicker(time.Second / 90)
	defer tk.Stop()
	start := time.Now()
	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			seq++
			t := now.Sub(start).Seconds()
			d.hub.Write(d.sample(seq, now, t))
		}
	}
}

func (d *SimDevice) sample(seq uint64, now time.Time, t float64) *gaze.Sample {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := &gaze.Sample{Seq: seq, HostTimeNs: now.UnixNano(), DeviceTimeUs: int64(t * 1e6),
		FrameCounter: uint32(seq), HasCounter: true}
	blink := math.Mod(t, 3.7) < 0.12
	sway := [3]float64{18 * math.Sin(t*0.31), 6 * math.Sin(t*0.23+1), 25 * math.Sin(t*0.17)}
	for i, side := range []float64{-1, 1} {
		e := &s.Left
		if i == 1 {
			e = &s.Right
		}
		p := [3]float64{side*31 + sway[0], 20 + sway[1], 620 + sway[2]}
		if blink {
			e.Status = 4
			continue
		}
		e.Tracked, e.OriginValid, e.PupilValid, e.TrackBoxValid = true, true, true, true
		e.OriginMM = [3]float32{float32(p[0]), float32(p[1]), float32(p[2])}
		// Come il firmware: lo stesso centro corneale nel sistema dello schermo.
		q := d.area.ToDisplay(p)
		e.OriginDisplayMM, e.OriginDisplayValid = [3]float32{float32(q[0]), float32(q[1]), float32(q[2])}, true
		e.PupilMM = float32(3.6 + 0.1*math.Sin(t))
		n := math.Sqrt(p[0]*p[0] + p[1]*p[1] + p[2]*p[2])
		e.TrackBox = [3]float32{float32(0.4998 - 1.3316*p[0]/p[2]), float32(0.4998 - 1.3316*p[1]/p[2]), float32((n - 400) / 500)}
	}
	// Sguardo: dopo ~180 ms di latenza saccadica raggiunge il punto con una
	// costante di tempo di 60 ms; senza punto vaga intorno al centro.
	goal := [2]float64{0.5 + 0.18*math.Sin(t*0.4), 0.45 + 0.12*math.Sin(t*0.53)}
	if d.visible && now.Sub(d.shownAt) > 180*time.Millisecond {
		goal = d.target
	}
	for k := 0; k < 2; k++ {
		d.gaze[k] += (goal[k] - d.gaze[k]) * (1 - math.Exp(-(1.0/90)/0.06))
	}
	if !blink && !d.uncalibrated {
		s.GazeValid, s.GazeRawValid = true, true
		for k := 0; k < 2; k++ {
			v := float32(d.gaze[k] + d.bias[k] + mrand.NormFloat64()*0.0025)
			s.Gaze[k], s.GazeRaw[k] = v, v
			s.Left.Gaze2D[k], s.Right.Gaze2D[k] = v, v
		}
		s.Left.Gaze2DValid, s.Right.Gaze2DValid = true, true
	} else {
		s.Gaze = [2]float32{-1, -1}
	}
	gaze.DeriveHead(s)
	return s
}
