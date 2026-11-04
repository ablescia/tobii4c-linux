package calib

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/store"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// Device è ciò che il controller chiede al tracker. La implementano la
// sessione USB reale (SessionDevice) e il simulatore (SimDevice).
type Device interface {
	Identity() []string
	DisplayInfo() (*ttp.DisplayArea, *ttp.Mounting)
	ReadCalibration() ([]byte, error)
	WriteCalibration([]byte) error
	WriteDisplayArea(ttp.DisplayArea) error
	Command(op uint32, payload []byte, timeout time.Duration) error
}

// SessionDevice adatta una tracker.Session a Device.
type SessionDevice struct{ *tracker.Session }

// Identity: seriale, modello, generazione, firmware.
func (d SessionDevice) Identity() []string { return d.DeviceStrings }

// pauseGrace: il primo frame dopo una sospensione arriva fino a ~200 ms dopo
// la risposta al comando.
const pauseGrace = time.Second

// Command invia un comando senza leggere la risposta. Lo stream può fermarsi
// mentre il dispositivo lo esegue: lo si dichiara al tracker.
func (d SessionDevice) Command(op uint32, payload []byte, timeout time.Duration) error {
	d.ExpectPause(timeout + pauseGrace)
	_, _, err := d.RequestTimeout(op, payload, timeout)
	d.ExpectPause(pauseGrace)
	return err
}

// ReadCalibration legge il blob dichiarando la possibile sospensione dello stream.
func (d SessionDevice) ReadCalibration() ([]byte, error) {
	d.ExpectPause(tracker.CalBlobTimeout + pauseGrace)
	defer d.ExpectPause(pauseGrace)
	return d.Session.ReadCalibration()
}

// WriteCalibration scrive il blob dichiarando la possibile sospensione dello stream.
func (d SessionDevice) WriteCalibration(b []byte) error {
	d.ExpectPause(tracker.CalBlobTimeout + pauseGrace)
	defer d.ExpectPause(pauseGrace)
	return d.Session.WriteCalibration(b)
}

// Stimulus riceve la posizione del punto mostrato (serve al simulatore).
type Stimulus interface {
	SetStimulus(x, y float64, visible bool)
}

// Config regola tempi e soglie del flusso.
type Config struct {
	FocusRadiusMM  float64       // lo sguardo deve cadere entro questo raggio dal punto...
	FocusSettle    time.Duration // ...per almeno questo tempo continuativo
	FocusTimeout   time.Duration // oltre, si raccoglie comunque se gli occhi sono tracciati
	BlinkTolerance time.Duration // buchi brevi (battiti di ciglia) non azzerano la fissazione
	// NoGazeDwell: occhi visti ma nessuno sguardo valido. Succede sul 4C dopo
	// il clear, finché il primo compute non produce una calibrazione: lì non
	// si può verificare la fissazione e si raccoglie dopo una sosta fissa,
	// come fa l'app Windows (che calcola subito dopo il primo punto proprio
	// per avere uno sguardo con cui guidare i punti successivi).
	NoGazeDwell      time.Duration
	ValidationSettle time.Duration
	ValidationWindow time.Duration
}

// DefaultConfig: fissazione di 0,5 s prima del collect, che a sua volta tiene
// lo sguardo sul punto per ~0,7 s (come l'app Windows, ~1 s in tutto).
func DefaultConfig() Config {
	return Config{
		FocusRadiusMM:    80,
		FocusSettle:      500 * time.Millisecond,
		FocusTimeout:     4 * time.Second,
		NoGazeDwell:      900 * time.Millisecond,
		BlinkTolerance:   200 * time.Millisecond,
		ValidationSettle: 600 * time.Millisecond,
		ValidationWindow: 1000 * time.Millisecond,
	}
}

// ValidationPoints: diversi da quelli di calibrazione, per non premiare
// l'adattamento ai soli punti raccolti.
var ValidationPoints = [][2]float64{{0.5, 0.5}, {0.25, 0.25}, {0.75, 0.25}, {0.75, 0.75}, {0.25, 0.75}}

// Controller esegue display setup, calibrazione e verifica.
type Controller struct {
	cfg Config
	log *slog.Logger
	Hub *Hub

	devMu sync.Mutex
	dev   Device
	stim  Stimulus

	opMu sync.Mutex // un'operazione sul dispositivo alla volta
	cal  CalState

	calID     atomic.Uint32
	calIDSeen atomic.Int64
}

// CalState è lo stato della calibrazione in questa esecuzione.
type CalState struct {
	Active     bool
	Collected  int
	Computes   int
	BackupPath string
	backup     []byte
	Finished   bool   // una calibrazione nuova è applicata
	SavedPath  string // dove è stata salvata
	SaveError  string
}

// New crea il controller.
func New(cfg Config, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, log: log, Hub: &Hub{}}
}

// SetDevice collega (o scollega con nil) il dispositivo corrente.
func (c *Controller) SetDevice(d Device) {
	c.devMu.Lock()
	c.dev = d
	if s, ok := d.(Stimulus); ok {
		c.stim = s
	}
	c.devMu.Unlock()
}

// TrackerHooks collega il controller al ciclo di vita del tracker reale.
func (c *Controller) TrackerHooks(o *tracker.Options) {
	o.OnReady = func(s *tracker.Session) error { c.SetDevice(SessionDevice{s}); return nil }
	o.OnClose = func(s *tracker.Session) {
		c.devMu.Lock()
		if sd, ok := c.dev.(SessionDevice); ok && sd.Session == s {
			c.dev = nil
		}
		c.devMu.Unlock()
		if c.opMu.TryLock() {
			if c.cal.Active {
				c.log.Warn("tracker disconnected during calibration")
				c.cal.Active = false
			}
			c.opMu.Unlock()
		}
	}
	o.OnNotification = c.Notify
}

// Notify riceve le notifiche del dispositivo (3220 = nuova calibrazione).
func (c *Controller) Notify(id uint32, payload []byte) {
	if id == ttp.NotifyCalibrationID {
		if v := ttp.ParseU32s(payload); len(v) > 0 {
			c.calID.Store(v[0])
			c.calIDSeen.Store(time.Now().UnixNano())
		}
	}
}

// ErrNoDevice: tracker non connesso.
var ErrNoDevice = errors.New("tracker non connesso")

// Device restituisce il dispositivo corrente.
func (c *Controller) Device() (Device, error) {
	c.devMu.Lock()
	defer c.devMu.Unlock()
	if c.dev == nil {
		return nil, ErrNoDevice
	}
	return c.dev, nil
}

// Show comunica al simulatore dove si trova il punto (no-op col tracker reale).
func (c *Controller) Show(x, y float64, visible bool) {
	c.devMu.Lock()
	s := c.stim
	c.devMu.Unlock()
	if s != nil {
		s.SetStimulus(x, y, visible)
	}
}

// State restituisce una copia dello stato di calibrazione.
func (c *Controller) State() CalState {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	return c.cal
}

// CalibrationID è l'ultimo id notificato dal dispositivo.
func (c *Controller) CalibrationID() uint32 { return c.calID.Load() }

// ApplyDisplay calcola la display area (tobii_calculate_display_area_basic)
// e la scrive, salvando la geometria in ~/.config/tobii4c/display.json.
func (c *Controller) ApplyDisplay(widthMM, heightMM, offsetXMM float64, monitor string) (ttp.DisplayArea, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.cal.Active {
		// Il dispositivo rifiuta la display area dentro una sessione di
		// calibrazione (BAD_STATE, osservato sull'ET5).
		return ttp.DisplayArea{}, errors.New("calibrazione in corso")
	}
	d, err := c.Device()
	if err != nil {
		return ttp.DisplayArea{}, err
	}
	_, m := d.DisplayInfo()
	if m == nil {
		return ttp.DisplayArea{}, errors.New("il tracker non ha fornito la geometria di montaggio")
	}
	area := ttp.DisplayAreaBasic(widthMM, heightMM, offsetXMM, *m)
	if err := d.WriteDisplayArea(area); err != nil {
		return area, fmt.Errorf("scrittura display area: %w", err)
	}
	if got, _ := d.DisplayInfo(); got != nil {
		area = *got
	}
	if err := store.SaveDisplay(store.Display{WidthMM: widthMM, HeightMM: heightMM, OffsetXMM: offsetXMM, Monitor: monitor, Area: &area}); err != nil {
		c.log.Warn("display.json not saved", "err", err)
	}
	c.log.Info("display area applied", "area", area.String())
	return area, nil
}

// Begin salva la calibrazione attiva, apre la sessione e la svuota.
func (c *Controller) Begin() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.cal.Active {
		return errors.New("calibrazione già in corso")
	}
	d, err := c.Device()
	if err != nil {
		return err
	}
	st := CalState{}
	if blob, err := d.ReadCalibration(); err == nil && len(blob) > 0 {
		st.backup = blob
		if p, err := store.SaveBackup(blob, "before-calibration"); err == nil {
			st.BackupPath = p
		} else {
			c.log.Warn("backup file not written", "err", err)
		}
	} else {
		c.log.Warn("no calibration backup available", "err", err)
	}
	if err := d.Command(ttp.OpCalStart, nil, tracker.CalCommandTimeout); err != nil {
		// Una sessione rimasta aperta da un'esecuzione interrotta: chiudi e riprova.
		var te *ttp.TTPError
		if errors.As(err, &te) && te.Code == ttp.ErrBadState {
			_ = d.Command(ttp.OpCalStop, nil, tracker.CalCommandTimeout)
			err = d.Command(ttp.OpCalStart, nil, tracker.CalCommandTimeout)
		}
		if err != nil {
			return fmt.Errorf("avvio calibrazione: %w", err)
		}
	}
	if err := d.Command(ttp.OpCalClear, nil, tracker.CalCommandTimeout); err != nil {
		_ = d.Command(ttp.OpCalStop, nil, tracker.CalCommandTimeout)
		return fmt.Errorf("azzeramento calibrazione: %w", err)
	}
	st.Active = true
	c.cal = st
	c.log.Info("calibration started", "backup_bytes", len(st.backup), "backup", st.BackupPath)
	return nil
}

// Outcome descrive l'esito di un punto di calibrazione.
type Outcome struct {
	OK           bool
	Reason       string // no_eyes, lost_eyes, device_refused
	Err          error
	Focused      bool
	Mode         string // focus (sguardo sul punto), dwell (nessuno sguardo: sosta fissa), timeout
	FocusTime    time.Duration
	CollectTime  time.Duration
	TrackedRatio float64
}

// waitFocus decide quando raccogliere un punto:
//   - "focus": lo sguardo è rimasto entro FocusRadiusMM per FocusSettle;
//   - "dwell": gli occhi sono visti con continuità per NoGazeDwell ma il
//     dispositivo non fornisce sguardo (nessuna calibrazione attiva);
//   - "timeout": sguardo presente ma mai sul punto entro FocusTimeout (display
//     area sbagliata o utente distratto): si raccoglie comunque se gli occhi
//     sono visti;
//   - "" (eyes=false): nessun occhio.
func (c *Controller) waitFocus(ctx context.Context, target [2]float64, area ttp.DisplayArea) (mode string, eyes bool, took time.Duration) {
	start := time.Now()
	tk := time.NewTicker(15 * time.Millisecond)
	defer tk.Stop()
	var since, lastIn, lastTracked, eyesSince, lastGaze time.Time
	for {
		select {
		case <-ctx.Done():
			return "", false, time.Since(start)
		case <-tk.C:
		}
		now := time.Now()
		s, ok := c.Hub.Latest()
		fresh := ok && now.Sub(time.Unix(0, s.HostTimeNs)) < 150*time.Millisecond
		tracked := fresh && (s.Left.Tracked || s.Right.Tracked)
		switch {
		case tracked:
			if eyesSince.IsZero() {
				eyesSince = now
			}
			lastTracked = now
		case now.Sub(lastTracked) > c.cfg.BlinkTolerance:
			eyesSince = time.Time{}
		}
		if tracked && s.GazeValid {
			lastGaze = now
		}
		in := tracked && s.GazeValid &&
			gazeDistanceMM([2]float64{float64(s.Gaze[0]), float64(s.Gaze[1])}, target, area) <= c.cfg.FocusRadiusMM
		switch {
		case in:
			if since.IsZero() {
				since = now
			}
			lastIn = now
		case now.Sub(lastIn) > c.cfg.BlinkTolerance:
			since = time.Time{}
		}
		if !since.IsZero() && now.Sub(since) >= c.cfg.FocusSettle {
			return "focus", true, now.Sub(start)
		}
		if !eyesSince.IsZero() && now.Sub(eyesSince) >= c.cfg.NoGazeDwell &&
			(lastGaze.IsZero() || now.Sub(lastGaze) >= c.cfg.NoGazeDwell) {
			return "dwell", true, now.Sub(start)
		}
		if now.Sub(start) >= c.cfg.FocusTimeout {
			return "timeout", !lastTracked.IsZero() && now.Sub(lastTracked) < time.Second, now.Sub(start)
		}
	}
}

func trackedRatio(samples []gaze.Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	n := 0
	for i := range samples {
		if samples[i].Left.Tracked || samples[i].Right.Tracked {
			n++
		}
	}
	return float64(n) / float64(len(samples))
}

// Collect attende che l'utente fissi il punto e lo fa raccogliere al
// dispositivo (op 1030, blocca ~0,7 s). Se gli occhi si perdono durante la
// raccolta il punto viene scartato (op 1080).
// onCollect (opzionale) viene chiamata quando la fissazione è confermata e
// parte il comando di raccolta.
func (c *Controller) Collect(ctx context.Context, x, y float64, onCollect func()) Outcome {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if !c.cal.Active {
		return Outcome{Err: errors.New("nessuna calibrazione in corso")}
	}
	d, err := c.Device()
	if err != nil {
		return Outcome{Err: err}
	}
	area, _ := d.DisplayInfo()
	if area == nil {
		return Outcome{Err: errors.New("display area sconosciuta")}
	}
	var o Outcome
	var eyes bool
	o.Mode, eyes, o.FocusTime = c.waitFocus(ctx, [2]float64{x, y}, *area)
	o.Focused = o.Mode == "focus"
	if ctx.Err() != nil {
		return Outcome{Err: ctx.Err()}
	}
	if !eyes {
		o.Reason = "no_eyes"
		return o
	}
	payload := ttp.CalPointPayload(float32(x), float32(y), ttp.EyesBoth)
	if onCollect != nil {
		onCollect()
	}
	t0 := time.Now()
	err = d.Command(ttp.OpCalCollect2D, payload, tracker.CalCollectTimeout)
	o.CollectTime = time.Since(t0)
	o.TrackedRatio = trackedRatio(c.Hub.Since(t0.UnixNano(), make([]gaze.Sample, 0, 128)))
	if err != nil {
		o.Reason, o.Err = "device_refused", err
		return o
	}
	if o.TrackedRatio < 0.5 {
		_ = d.Command(ttp.OpCalDiscard2D, payload, tracker.CalCommandTimeout)
		o.Reason = "lost_eyes"
		return o
	}
	c.cal.Collected++
	o.OK = true
	c.log.Info("calibration point collected", "x", fmt.Sprintf("%.2f", x), "y", fmt.Sprintf("%.2f", y), "mode", o.Mode,
		"focus_ms", o.FocusTime.Milliseconds(), "collect_ms", o.CollectTime.Milliseconds(),
		"tracked", fmt.Sprintf("%.0f%%", o.TrackedRatio*100))
	return o
}

// Compute calcola e applica la calibrazione con i punti raccolti (op 1070).
func (c *Controller) Compute() (time.Duration, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if !c.cal.Active {
		return 0, errors.New("nessuna calibrazione in corso")
	}
	d, err := c.Device()
	if err != nil {
		return 0, err
	}
	before := c.calIDSeen.Load()
	t0 := time.Now()
	if err := d.Command(ttp.OpCalCompute, nil, tracker.CalComputeTimeout); err != nil {
		return time.Since(t0), fmt.Errorf("calcolo calibrazione: %w", err)
	}
	took := time.Since(t0)
	for i := 0; i < 40 && c.calIDSeen.Load() == before; i++ {
		time.Sleep(25 * time.Millisecond) // la notifica 3220 può seguire la risposta
	}
	c.cal.Computes++
	c.log.Info("calibration computed", "ms", took.Milliseconds(), "calibration_id", c.calID.Load(), "points", c.cal.Collected)
	return took, nil
}

// Finish legge la nuova calibrazione (dentro la sessione, come Windows),
// chiude la sessione e salva il blob in ~/.config/tobii4c/calibration.bin.
func (c *Controller) Finish() (CalState, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if !c.cal.Active {
		return c.cal, errors.New("nessuna calibrazione in corso")
	}
	if c.cal.Computes == 0 {
		return c.cal, errors.New("nessun calcolo eseguito")
	}
	d, err := c.Device()
	if err != nil {
		return c.cal, err
	}
	blob, rerr := d.ReadCalibration()
	if err := d.Command(ttp.OpCalStop, nil, tracker.CalCommandTimeout); err != nil {
		c.log.Warn("calibration stop failed", "err", err)
	}
	c.cal.Active, c.cal.Finished = false, true
	if rerr != nil {
		c.cal.SaveError = rerr.Error()
		return c.cal, nil
	}
	p, err := store.CalibrationPath()
	if err == nil {
		if old, rerr := os.ReadFile(p); rerr == nil {
			_, _ = store.SaveBackup(old, "previous-file")
		}
		err = store.WriteAtomic(p, blob)
	}
	if err != nil {
		c.cal.SaveError = err.Error()
	} else {
		c.cal.SavedPath = p
	}
	c.log.Info("calibration finished", "bytes", len(blob), "saved", c.cal.SavedPath)
	return c.cal, nil
}

// Abort interrompe una calibrazione in corso rimettendo quella precedente.
func (c *Controller) Abort() (restored bool, err error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	return c.abortLocked()
}

func (c *Controller) abortLocked() (bool, error) {
	if !c.cal.Active {
		return false, nil
	}
	c.cal.Active = false
	d, err := c.Device()
	if err != nil {
		return false, err
	}
	restored := false
	if len(c.cal.backup) > 0 {
		restored = d.WriteCalibration(c.cal.backup) == nil
	}
	_ = d.Command(ttp.OpCalStop, nil, tracker.CalCommandTimeout)
	if !restored && len(c.cal.backup) > 0 {
		restored = d.WriteCalibration(c.cal.backup) == nil
	}
	c.log.Info("calibration aborted", "restored_previous", restored)
	return restored, nil
}

// CanRevert indica se in questa esecuzione c'è una calibrazione precedente da rimettere.
func (c *Controller) CanRevert() bool {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	return c.cal.Finished && len(c.cal.backup) > 0
}

// Revert rimette sul dispositivo (e nel file) la calibrazione precedente.
func (c *Controller) Revert() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.cal.Active {
		_, err := c.abortLocked()
		return err
	}
	if len(c.cal.backup) == 0 {
		return errors.New("nessuna calibrazione precedente in questa esecuzione")
	}
	d, err := c.Device()
	if err != nil {
		return err
	}
	if err := d.WriteCalibration(c.cal.backup); err != nil {
		return fmt.Errorf("ripristino: %w", err)
	}
	if p, err := store.CalibrationPath(); err == nil {
		_ = store.WriteAtomic(p, c.cal.backup)
	}
	c.cal.Finished = false
	c.log.Info("previous calibration restored", "bytes", len(c.cal.backup))
	return nil
}

// AbortIfActive va chiamata all'uscita: il dispositivo non resta mai a metà.
func (c *Controller) AbortIfActive() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.cal.Active {
		_, _ = c.abortLocked()
	}
}

// Validate misura accuratezza e precisione su un punto: attende che lo
// sguardo si assesti, poi analizza una finestra di campioni.
func (c *Controller) Validate(ctx context.Context, x, y float64) (PointResult, error) {
	d, err := c.Device()
	if err != nil {
		return PointResult{}, err
	}
	area, _ := d.DisplayInfo()
	if area == nil {
		return PointResult{}, errors.New("display area sconosciuta")
	}
	select {
	case <-time.After(c.cfg.ValidationSettle):
	case <-ctx.Done():
		return PointResult{}, ctx.Err()
	}
	t0 := time.Now()
	select {
	case <-time.After(c.cfg.ValidationWindow):
	case <-ctx.Done():
		return PointResult{}, ctx.Err()
	}
	res := Evaluate([2]float64{x, y}, c.Hub.Since(t0.UnixNano(), make([]gaze.Sample, 0, 128)), *area)
	c.log.Info("validation point", "target", res.Target, "n", res.N,
		"error_deg", fmt.Sprintf("%.2f", res.ErrorDeg), "precision_deg", fmt.Sprintf("%.2f", res.PrecisionMS))
	return res, nil
}
