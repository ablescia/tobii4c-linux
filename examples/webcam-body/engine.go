//go:build linux

package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/bodypose"
	"github.com/ablescia/tobii4c-linux/internal/tflite"
	"github.com/ablescia/tobii4c-linux/internal/webcam"
)

// Pipeline: cattura → elaborazione → interfaccia, come webcam-face.
//
//   - cattura: preleva i buffer dal driver, ne ricava i piani YCbCr in un
//     frame del pool e lo passa all'elaborazione; se questa è occupata il
//     frame in attesa viene sostituito da quello nuovo (conta come
//     scartato), così la latenza non cresce.
//   - elaborazione: la pipeline di MediaPipe Pose sul tracker scelto (GPU o
//     CPU, corpo intero o mezzo busto), le misure, la pubblicazione
//     dell'ultimo risultato. I cambi di configurazione chiesti
//     dall'interfaccia si applicano qui, tra un frame e l'altro (caricando
//     o scaricando i modelli se serve): l'anteprima continua con i frame
//     della cattura.
//   - interfaccia: legge l'ultimo risultato con Snapshot.

const (
	histLen    = 1024 // punti della storia (~17 s a 60 Hz)
	poolFrames = 3
)

// Config è la configurazione del rilevamento.
type Config struct {
	Mode    bodypose.Mode
	Variant string // lite, full, heavy (solo corpo intero)
	GPU     bool
}

func (c Config) key() Config {
	if c.Mode == bodypose.UpperBody {
		c.Variant = ""
	}
	return c
}

func (c Config) String() string {
	s := c.Mode.String()
	if c.Mode == bodypose.FullBody {
		s += " " + c.Variant
	}
	if c.GPU {
		return s + " · GPU"
	}
	return s + " · CPU"
}

type frame struct {
	webcam.Frame
	seq      uint64
	camSeq   uint32
	captured time.Time
	latency  time.Duration
	bytes    int
}

// Result è l'ultimo frame elaborato.
type Result struct {
	frame webcam.Frame // piani del frame elaborato

	Seq       uint64
	CamSeq    uint32
	Captured  time.Time
	Latency   time.Duration
	Processed time.Time
	Bytes     int

	Cfg     Config
	Pose    bodypose.Pose // Mask è una copia propria
	M       bodypose.Metrics
	DetMs   float64 // detector: tempo totale e tempo GPU (0 se non eseguito)
	DetGPU  float64
	LmMs    float64
	LmGPU   float64
	PostMs  float64
	TotalMs float64 // dall'inizio alla fine dell'elaborazione del frame
	InfMean float64 // TotalMs: ultimi 120 frame
	InfP95  float64
	InfMax  float64
}

// HistPoint è un punto della storia per i grafici.
type HistPoint struct {
	T       time.Time
	Found   bool
	Score   float64
	Joints  [bodypose.NumJoints]float64 // NaN se non visibile
	Lean    float64                     // NaN se non misurato
	Tilt    float64
	Forward float64
	TotalMs float64
	GPUMs   float64 // 0 sulla CPU
	DetRan  bool
}

// Counters sono i contatori cumulativi della pipeline.
type Counters struct {
	Captured, BusyDrops, SeqGaps, BufErrors, DecodeErrs, Bytes uint64
	Processed, Found, Detections, Switches                     uint64
	DecodeNs, LatencyNs, PipelineNs                            int64
}

// ModelInfo descrive un tracker caricato.
type ModelInfo struct {
	Spec          bodypose.Spec
	Files         []string
	DetStats      string
	LmStats       string
	DetMACs       int64
	LmMACs        int64
	DetNodes      int
	LmNodes       int
	WeightsMB     float64
	ArenaMB       float64
	LoadTime      time.Duration
	LandmarkInput int
}

// Engine possiede webcam, backend, modelli e stato condiviso.
type Engine struct {
	Info  webcam.CamInfo
	Focal float64 // pixel
	FOV   float64 // gradi

	GPU    *tflite.GPU // nil se Vulkan non è disponibile
	GPUErr string
	CPU    *tflite.CPU

	cam       *webcam.Webcam
	conv      *webcam.Converter
	modelsDir string
	offline   bool

	free chan *frame
	work chan *frame

	captured, busyDrops, seqGaps, bufErrors, decodeErrs, bytes atomic.Uint64
	processed, found, detections, switches                     atomic.Uint64
	decodeNs, latencyNs, pipelineNs                            atomic.Int64

	mu       sync.Mutex
	res      Result
	have     bool
	raw      webcam.Frame // ultimo frame catturato, per l'anteprima
	rawSeq   uint64
	hist     [histLen]HistPoint
	histN    int
	ctrls    []Control
	camErr   string
	camErrAt time.Time
	want     Config // configurazione chiesta dall'interfaccia
	cur      Config // in uso
	loading  string // messaggio durante un cambio
	loadErr  string
	infos    map[Config]ModelInfo

	// solo la goroutine dell'elaborazione
	trackers map[Config]*bodypose.Tracker
	timing   timing
}

// Control è un controllo V4L2 (alias per l'interfaccia).
type Control = webcam.Control

// NewEngine prepara la pipeline su una webcam aperta; decoders sono le
// goroutine della decodifica MJPEG.
func NewEngine(cam *webcam.Webcam, gpu *tflite.GPU, gpuErr string, cpu *tflite.CPU, focal, fov float64, decoders int,
	modelsDir string, offline bool) *Engine {
	in := cam.Info
	e := &Engine{Info: in, cam: cam, conv: webcam.NewConverter(&in, decoders), Focal: focal, FOV: fov,
		GPU: gpu, GPUErr: gpuErr, CPU: cpu, modelsDir: modelsDir, offline: offline,
		free: make(chan *frame, poolFrames), work: make(chan *frame, 1),
		trackers: map[Config]*bodypose.Tracker{}, infos: map[Config]ModelInfo{}}
	for i := 0; i < poolFrames; i++ {
		e.free <- &frame{Frame: *webcam.NewFrame(in.W, in.H, true)}
	}
	e.raw = *webcam.NewFrame(in.W, in.H, true)
	e.ctrls = cam.Controls()
	return e
}

// Prepare carica i modelli della configurazione iniziale (prima di Run).
func (e *Engine) Prepare(c Config, progress bodypose.Progress) error {
	if c.GPU && e.GPU == nil {
		c.GPU = false
	}
	if _, err := e.tracker(c.key(), progress); err != nil {
		return err
	}
	e.mu.Lock()
	e.want, e.cur = c, c
	e.mu.Unlock()
	return nil
}

// Request chiede un cambio di configurazione (applicato al frame successivo).
func (e *Engine) Request(c Config) {
	if c.GPU && e.GPU == nil {
		c.GPU = false
	}
	e.mu.Lock()
	e.want = c
	e.mu.Unlock()
}

// Wanted è la configurazione chiesta (anche se non ancora in uso).
func (e *Engine) Wanted() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.want
}

// tracker restituisce il tracker di c (una chiave: Config.key), caricandolo
// se serve.
func (e *Engine) tracker(c Config, progress bodypose.Progress) (*bodypose.Tracker, error) {
	if t, ok := e.trackers[c]; ok {
		return t, nil
	}
	t0 := time.Now()
	m, err := bodypose.Load(c.Mode, c.Variant, e.modelsDir, e.offline, progress)
	if err != nil {
		return nil, err
	}
	var b tflite.Backend = e.CPU
	if c.GPU {
		b = e.GPU
	}
	if progress != nil {
		progress("preparo " + m.Spec.Title + " sulla " + b.Name())
	}
	t, err := bodypose.NewTracker(b, m)
	if err != nil {
		return nil, err
	}
	dg, lg := t.Graphs()
	info := ModelInfo{Spec: m.Spec, Files: m.Files, DetStats: dg.Stats(), LmStats: lg.Stats(), DetMACs: dg.MACs, LmMACs: lg.MACs,
		DetNodes: len(dg.Nodes), LmNodes: len(lg.Nodes), WeightsMB: float64((dg.Weights+lg.Weights)*4) / (1 << 20),
		ArenaMB: float64((dg.Arena-dg.Weights+lg.Arena-lg.Weights)*4) / (1 << 20), LoadTime: time.Since(t0),
		LandmarkInput: lg.InputShape()[1]}
	e.trackers[c] = t
	e.mu.Lock()
	e.infos[c] = info
	e.mu.Unlock()
	return t, nil
}

// ModelInfo restituisce la descrizione del tracker di c (se caricato).
func (e *Engine) ModelInfo(c Config) (ModelInfo, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	i, ok := e.infos[c.key()]
	return i, ok
}

// Run esegue cattura ed elaborazione finché ctx non termina o la webcam si
// stacca.
func (e *Engine) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); defer cancel(); e.capture(ctx) }()
	go func() { defer wg.Done(); e.process(ctx) }()
	go func() { defer wg.Done(); e.pollControls(ctx) }()
	wg.Wait()
	e.conv.Close()
}

// ConvNote descrive la conversione dei frame.
func (e *Engine) ConvNote() string { return e.conv.Note() }

func (e *Engine) setErr(s string) {
	e.mu.Lock()
	if s != "" && e.camErr != s {
		e.camErrAt = time.Now()
	}
	e.camErr = s
	e.mu.Unlock()
}

func (e *Engine) capture(ctx context.Context) {
	var lastSeq uint32
	haveSeq := false
	var seq uint64
	for ctx.Err() == nil {
		var f *frame
		select {
		case f = <-e.free:
		case <-ctx.Done():
			return
		}
		var convErr error
		bad := false
		err := e.cam.Next(time.Second, func(data []byte, bi webcam.BufInfo) {
			t0 := time.Now()
			if haveSeq && bi.Sequence > lastSeq+1 {
				e.seqGaps.Add(uint64(bi.Sequence - lastSeq - 1))
			}
			lastSeq, haveSeq = bi.Sequence, true
			if bi.Flags&webcam.BufFlagError != 0 {
				e.bufErrors.Add(1)
				bad = true
				return
			}
			convErr = e.conv.Convert(data, &f.Frame, true)
			e.decodeNs.Add(time.Since(t0).Nanoseconds())
			f.camSeq, f.bytes, f.captured = bi.Sequence, len(data), t0
			f.latency = 0
			if bi.Monotonic() {
				f.latency = bi.Dequeued - bi.Timestamp
			}
		})
		switch {
		case err == webcam.ErrTimeout:
			e.setErr("nessun frame dalla webcam da più di un secondo")
			e.free <- f
			continue
		case err != nil:
			e.setErr("webcam: " + err.Error())
			e.free <- f
			return
		case bad:
			e.free <- f
			continue
		case convErr != nil:
			e.decodeErrs.Add(1)
			e.setErr("conversione: " + convErr.Error())
			e.free <- f
			continue
		}
		e.setErr("")
		seq++
		f.seq = seq
		e.mu.Lock()
		e.raw.CopyFrom(&f.Frame)
		e.rawSeq = seq
		e.mu.Unlock()
		e.captured.Add(1)
		e.bytes.Add(uint64(f.bytes))
		e.latencyNs.Add(f.latency.Nanoseconds())
		select {
		case e.work <- f:
		default:
			// elaborazione occupata: il frame nuovo sostituisce quello in attesa
			select {
			case old := <-e.work:
				e.busyDrops.Add(1)
				e.free <- old
			default:
			}
			e.work <- f
		}
	}
}

func (e *Engine) process(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-e.work:
			e.switchIfNeeded()
			e.handle(f)
			e.free <- f
		}
	}
}

// switchIfNeeded applica la configurazione chiesta, caricando i modelli se
// mancano (intanto l'anteprima mostra i frame della cattura).
func (e *Engine) switchIfNeeded() {
	e.mu.Lock()
	want, cur := e.want, e.cur
	if want.key() == cur.key() {
		e.cur = want // stessa rete: conta solo la variante ricordata
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	progress := func(msg string) {
		e.mu.Lock()
		e.loading = msg
		e.mu.Unlock()
	}
	progress("carico " + want.key().String() + "…")
	t, err := e.tracker(want.key(), progress)
	e.mu.Lock()
	e.loading = ""
	if err != nil {
		e.loadErr = want.key().String() + ": " + err.Error()
		e.want = e.cur // torna alla configurazione che funziona
		e.mu.Unlock()
		return
	}
	e.loadErr = ""
	e.cur = want
	e.mu.Unlock()
	t.Reset()
	e.timing = timing{}
	e.switches.Add(1)
}

func (e *Engine) handle(f *frame) {
	e.mu.Lock()
	cfg := e.cur
	e.mu.Unlock()
	t := e.trackers[cfg.key()]
	if t == nil {
		return
	}
	img := tflite.Image{W: f.W, H: f.H, Y: f.Y, CW: f.CW, CH: f.CH, Cb: f.Cb, Cr: f.Cr}
	p, err := t.Process(&img, f.captured)
	if err != nil {
		e.setErr("modello: " + err.Error())
		return
	}
	m := bodypose.Measure(p, e.Focal)
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	total := ms(p.Times.Total)
	e.timing.add(total)
	mean, p95, mx := e.timing.stats()
	e.processed.Add(1)
	if p.Found {
		e.found.Add(1)
	}
	if p.DetRan {
		e.detections.Add(1)
	}
	now := time.Now()
	e.pipelineNs.Add(now.Sub(f.captured).Nanoseconds())

	hp := HistPoint{T: f.captured, Found: p.Found, Score: p.Score, TotalMs: total, DetRan: p.DetRan,
		Lean: nan, Tilt: nan, Forward: nan}
	if cfg.GPU {
		hp.GPUMs = ms(p.Times.Detect.GPU + p.Times.Landmarks.GPU)
	}
	for j := range hp.Joints {
		hp.Joints[j] = nan
		if m.Joints[j].OK {
			hp.Joints[j] = m.Joints[j].Deg
		}
	}
	if m.LeanOK {
		hp.Lean = m.LeanDeg
	}
	if m.ShouldersOK {
		hp.Tilt = m.ShoulderTiltDeg
	}
	if m.ForwardOK {
		hp.Forward = m.ForwardDeg
	}

	e.mu.Lock()
	r := &e.res
	mask := r.Pose.Mask
	pix := r.frame
	*r = Result{Seq: f.seq, CamSeq: f.camSeq, Captured: f.captured, Latency: f.latency, Processed: now, Bytes: f.bytes,
		Cfg: cfg, Pose: *p, M: m, DetMs: ms(p.Times.Detect.Wall), DetGPU: ms(p.Times.Detect.GPU),
		LmMs: ms(p.Times.Landmarks.Wall), LmGPU: ms(p.Times.Landmarks.GPU), PostMs: ms(p.Times.Post), TotalMs: total,
		InfMean: mean, InfP95: p95, InfMax: mx, frame: pix}
	r.Pose.Mask = append(mask[:0], p.Mask...)
	r.frame.CopyFrom(&f.Frame)
	e.hist[e.histN%histLen] = hp
	e.histN++
	e.have = true
	e.mu.Unlock()
}

// pollControls rilegge i controlli della webcam (l'esposizione automatica
// li cambia): ogni lettura è un trasferimento USB, per questo sta fuori
// dalla cattura.
func (e *Engine) pollControls(ctx context.Context) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	e.mu.Lock()
	cs := append([]Control(nil), e.ctrls...)
	e.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		e.cam.ReadControls(cs)
		e.mu.Lock()
		e.ctrls = append(e.ctrls[:0], cs...)
		e.mu.Unlock()
	}
}

// Snapshot è la copia dello stato per l'interfaccia.
type Snapshot struct {
	Res  Result // ultimo frame elaborato
	Have bool

	// Immagine dell'anteprima: di norma il frame elaborato (Overlay: lo
	// scheletro di Res è il suo); se il modello è indietro di almeno due
	// frame (caricamento di un modello) l'ultimo catturato, senza
	// sovrapposizioni.
	Pix      webcam.Frame
	PixSeq   uint64
	Overlay  bool
	Hist     []HistPoint // dal più vecchio
	C        Counters
	Ctrls    []Control
	Err      string
	ErrSince time.Time
	Want     Config
	Cur      Config
	Loading  string
	LoadErr  string
}

// Snapshot copia lo stato in dst; restituisce true se l'immagine
// dell'anteprima è cambiata. Non torna mai a un frame più vecchio di quello
// già mostrato.
func (e *Engine) Snapshot(dst *Snapshot) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.have {
		mask := dst.Res.Pose.Mask
		pix := dst.Res.frame
		dst.Res = e.res
		dst.Res.Pose.Mask = append(mask[:0], e.res.Pose.Mask...)
		dst.Res.frame = pix
	}
	dst.Have = e.have
	live := !e.have || e.rawSeq >= e.res.Seq+2
	seq, src := e.res.Seq, &e.res.frame
	if live {
		seq, src = e.rawSeq, &e.raw
	}
	fresh := seq > dst.PixSeq && src.W > 0 && len(src.Y) >= src.W*src.H
	if fresh {
		dst.Pix.CopyFrom(src)
		dst.PixSeq, dst.Overlay = seq, !live
	}
	n := min(e.histN, histLen)
	dst.Hist = dst.Hist[:0]
	for i := e.histN - n; i < e.histN; i++ {
		dst.Hist = append(dst.Hist, e.hist[i%histLen])
	}
	dst.Ctrls = append(dst.Ctrls[:0], e.ctrls...)
	dst.Err, dst.ErrSince = e.camErr, e.camErrAt
	dst.Want, dst.Cur, dst.Loading, dst.LoadErr = e.want, e.cur, e.loading, e.loadErr
	dst.C = Counters{
		Captured: e.captured.Load(), BusyDrops: e.busyDrops.Load(), SeqGaps: e.seqGaps.Load(),
		BufErrors: e.bufErrors.Load(), DecodeErrs: e.decodeErrs.Load(), Bytes: e.bytes.Load(),
		Processed: e.processed.Load(), Found: e.found.Load(), Detections: e.detections.Load(), Switches: e.switches.Load(),
		DecodeNs: e.decodeNs.Load(), LatencyNs: e.latencyNs.Load(), PipelineNs: e.pipelineNs.Load(),
	}
	return fresh
}

// timing tiene gli ultimi 120 tempi per frame.
type timing struct {
	v   [120]float64
	n   int
	tmp [120]float64
}

func (t *timing) add(ms float64) {
	t.v[t.n%len(t.v)] = ms
	t.n++
}

func (t *timing) stats() (mean, p95, mx float64) {
	n := min(t.n, len(t.v))
	if n == 0 {
		return
	}
	s := t.tmp[:n]
	copy(s, t.v[:n])
	sort.Float64s(s)
	for _, v := range s {
		mean += v
	}
	return mean / float64(n), s[min(n-1, n*95/100)], s[n-1]
}

// Close libera i modelli e i backend.
func (e *Engine) Close() {
	for _, t := range e.trackers {
		t.Close()
	}
	if e.GPU != nil {
		e.GPU.Close()
	}
	e.CPU.Close()
}

func (e *Engine) describeBackend(gpu bool) string {
	if gpu && e.GPU != nil {
		in := e.GPU.Info()
		return fmt.Sprintf("%s (%s %s)", in.Name, in.Driver, in.DriverInfo)
	}
	return "CPU · " + e.CPU.Device()
}
