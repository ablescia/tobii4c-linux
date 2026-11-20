package headpose

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/ablescia/tobii4c-linux/internal/irimg"
)

// API per l'applicazione guidata (tobii4c headpose-calibrate): raccolta
// controllata dei campioni, calibrazione su richiesta, anteprima della camera,
// zero personale del pitch.

// Coverage descrive i campioni raccolti per la calibrazione.
type Coverage struct {
	Samples, Cells int
	// DistBins conta i campioni per fasce di 5 cm di distanza dal tracker,
	// da 40 a 100 cm.
	DistBins [DistBinCount]int
	DistMin  float64 // mm
	DistMax  float64
}

// Fasce di distanza della copertura.
const (
	DistBinCount = 12
	DistBinMinMM = 400.0
	DistBinMM    = 50.0
)

func (e *Estimator) updateCoverage(mid vec3, samples, cells int) {
	d := norm(mid)
	e.mu.Lock()
	defer e.mu.Unlock()
	c := &e.cov
	if c.Samples == 0 || d < c.DistMin {
		c.DistMin = d
	}
	if d > c.DistMax {
		c.DistMax = d
	}
	c.Samples, c.Cells = samples, cells
	if b := int((d - DistBinMinMM) / DistBinMM); b >= 0 && b < DistBinCount {
		c.DistBins[b]++
	}
}

// Coverage restituisce la copertura attuale.
func (e *Estimator) Coverage() Coverage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cov
}

// do esegue fn nella goroutine di lavoro e attende che finisca.
func (e *Estimator) do(fn func()) {
	done := make(chan struct{})
	select {
	case e.ctl <- func() { fn(); close(done) }:
		select {
		case <-done:
		case <-e.done:
		}
	case <-e.done:
	}
}

// ResetSamples svuota il serbatoio della calibrazione.
func (e *Estimator) ResetSamples() {
	e.do(func() {
		e.samples = e.samples[:0]
		e.voxels = map[[3]int][]int{}
		e.newVoxels = 0
	})
	e.mu.Lock()
	e.cov = Coverage{}
	e.mu.Unlock()
}

// CalibrateNow stima la camera dai campioni raccolti, in background, e
// consegna il risultato a done (da un'altra goroutine).
func (e *Estimator) CalibrateNow(done func(*Camera, CalibQuality, error)) {
	var snap []CalibSample
	var w, h int
	e.do(func() {
		snap = append(snap, e.samples...)
		w, h = e.frame.W, e.frame.H
	})
	if w == 0 {
		w, h = 640, 480
	}
	go func() {
		if len(snap) < 60 {
			done(nil, CalibQuality{}, fmt.Errorf("troppo pochi campioni (%d)", len(snap)))
			return
		}
		cam, q, err := calibrateFast(snap, w, h)
		done(cam, q, err)
	}()
}

// CalibrationReliable dice se una calibrazione supera le soglie del driver.
func CalibrationReliable(q CalibQuality) bool {
	return q.RMSPx <= acceptRMSPx && q.SigmaDeg <= acceptSigmaDeg
}

// Adopt installa una calibrazione della camera e, se save, la salva.
func (e *Estimator) Adopt(cam *Camera, q CalibQuality, save bool) error {
	e.do(func() {
		e.wcam, e.wq, e.track = cam, q, false
		e.filt.reset()
	})
	e.log.Info("head pose: camera calibration adopted", "focal_px", round1(cam.F),
		"pp", fmt.Sprintf("%.0f,%.0f", cam.CX, cam.CY), "tilt_deg", round1(cameraTilt(cam)),
		"rms_px", round2(q.RMSPx), "sigma_deg", round2(q.SigmaDeg), "mirror", cam.Mirror)
	if !save || e.cfg.CameraPath == "" {
		return nil
	}
	e.mu.Lock()
	serial := e.serial
	e.mu.Unlock()
	return cam.Save(e.cfg.CameraPath, serial, q)
}

// CameraTiltDeg è l'elevazione dell'asse ottico nel sistema del tracker.
func CameraTiltDeg(c *Camera) float64 { return cameraTilt(c) }

// Status riassume ciò che lo stimatore sa: camera calibrata (con qualità) e
// zero personale del pitch.
type Status struct {
	Calibrated bool
	Quality    CalibQuality
	Camera     *Camera
	HasOffset  bool
	Offset     float64 // rad
}

// Status restituisce lo stato attuale.
func (e *Estimator) Status() Status {
	var st Status
	e.do(func() {
		e.mu.Lock()
		if e.camNew { // calibrazione appena caricata da SetDevice
			e.wcam, e.wq, e.camNew = e.cam, e.camQ, false
		}
		e.mu.Unlock()
		st.Calibrated, st.Quality, st.Camera = e.wcam != nil, e.wq, e.wcam
	})
	e.mu.Lock()
	st.HasOffset, st.Offset = e.hasOffset, e.offset
	e.mu.Unlock()
	return st
}

// SetPitchOffset imposta lo zero personale del pitch (rad: il pitch grezzo
// misurato con la testa dritta) e, se UserPath è configurato, lo salva.
func (e *Estimator) SetPitchOffset(rad float64) error {
	e.mu.Lock()
	e.offset, e.hasOffset = rad, true
	e.mu.Unlock()
	if e.cfg.UserPath == "" {
		return nil
	}
	data, _ := json.MarshalIndent(map[string]float64{"pitch_offset_deg": deg(rad)}, "", "  ")
	tmp := e.cfg.UserPath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, e.cfg.UserPath)
}

// PitchOffset restituisce lo zero personale del pitch (rad).
func (e *Estimator) PitchOffset() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.offset
}

// LoadPitchOffset legge lo zero personale del pitch (rad).
func LoadPitchOffset(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var v map[string]float64
	if err := json.Unmarshal(data, &v); err != nil {
		return 0, err
	}
	d, ok := v["pitch_offset_deg"]
	if !ok || math.IsNaN(d) || math.Abs(d) > 60 {
		return 0, errors.New("invalid pitch offset")
	}
	return d * math.Pi / 180, nil
}

// Preview è l'ultima immagine elaborata, con i landmark se c'è un viso.
type Preview struct {
	Seq      uint64
	W, H     int
	Pix      []byte
	Face     bool
	LM       Landmarks
	Crop     Crop
	Score    float64
	DeviceUs int64
	Mirror   bool // l'immagine è già stata ribaltata (camera speculare)
}

func (e *Estimator) publishPreview(f *irimg.Frame, lm *Landmarks, c Crop, res Result) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := &e.prev
	p.Seq++
	p.W, p.H = f.W, f.H
	p.Pix = append(p.Pix[:0], f.Pix[:f.W*f.H]...)
	p.Face = lm != nil
	if lm != nil {
		p.LM = *lm
	}
	p.Crop, p.Score, p.DeviceUs = c, res.Score, f.DeviceUs
	p.Mirror = e.wcam != nil && e.wcam.Mirror
}

// LatestPreview copia in dst l'ultima immagine se è più recente di dst.Seq.
func (e *Estimator) LatestPreview(dst *Preview) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.prev.Seq == dst.Seq {
		return false
	}
	pix := append(dst.Pix[:0], e.prev.Pix...)
	*dst = e.prev
	dst.Pix = pix
	return true
}
