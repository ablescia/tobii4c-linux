//go:build linux

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/webcam"
)

// Pipeline: cattura → elaborazione → interfaccia.
//
//   - cattura: preleva i buffer dal driver, ne copia la luminanza in un frame
//     del pool e lo passa all'elaborazione; se questa è ancora occupata, il
//     frame in attesa viene sostituito da quello nuovo (conta come scartato),
//     così la latenza non cresce.
//   - elaborazione: modello del viso (inseguimento dal frame precedente o
//     ricerca a griglia), misure, pubblicazione dell'ultimo risultato.
//   - interfaccia: legge l'ultimo risultato con Snapshot.

const (
	minFaceScore = 0.5                     // come il driver
	trackWindow  = 1500 * time.Millisecond // inseguimento dall'ultimo viso trovato
	histLen      = 1024                    // punti della storia (~30 s a 30 Hz)
	poolFrames   = 3
)

// frame è un'immagine in scala di grigi con i dati della cattura.
type frame struct {
	irimg.Frame
	seq      uint64 // numero del frame nell'applicazione
	camSeq   uint32 // sequence del driver
	captured time.Time
	latency  time.Duration // dal timestamp del driver al prelievo
	bytes    int
}

// Result è l'ultimo frame elaborato.
type Result struct {
	Seq       uint64
	CamSeq    uint32
	W, H      int
	Pix       []byte
	Captured  time.Time
	Latency   time.Duration // driver → applicazione
	Processed time.Time
	Bytes     int

	Face     bool
	Score    float64
	Mode     string
	Crop     headpose.Crop
	LM       headpose.Landmarks
	InferMs  float64 // tempo del modello su questo frame
	InfMean  float64 // ultimi 120 frame
	InfP95   float64
	InfMax   float64
	SearchMs float64 // durata dell'ultima ricerca a griglia
	M        Metrics
	Img      ImageStats

	Closed    bool // occhi chiusi in questo frame
	BlinkBase float64
	BlinksMin int
	BlinkDur  time.Duration
}

// HistPoint è un punto della storia per i grafici.
type HistPoint struct {
	T                time.Time
	Ran              bool // il modello è stato eseguito su questo frame
	Face, PoseOK     bool
	Pitch, Yaw, Roll float64 // gradi
	EAR              [2]float64
	InferMs          float64
	Score            float64
	DistMM           float64
}

// Counters sono i contatori cumulativi della pipeline.
type Counters struct {
	Captured, BusyDrops, SeqGaps, BufErrors, DecodeErrs, Bytes uint64
	Processed, Faces, Searches, Blinks                         uint64
	DecodeNs, LatencyNs, PipelineNs                            int64
}

// Control è un controllo V4L2 (alias per l'interfaccia).
type Control = webcam.Control

// Engine possiede la webcam, il modello e lo stato condiviso.
type Engine struct {
	Info    webcam.CamInfo
	Focal   float64 // pixel
	FOV     float64 // gradi
	Workers int
	MACs    int64
	Model   string
	ModelOK bool // SHA-256 del modello provato dal driver

	cam  *webcam.Webcam
	det  *headpose.Detector
	conv *webcam.Converter

	free chan *frame
	work chan *frame

	captured, busyDrops, seqGaps, bufErrors, decodeErrs, bytes atomic.Uint64
	processed, faces, searches, blinks                         atomic.Uint64
	decodeNs, latencyNs, pipelineNs                            atomic.Int64

	searching atomic.Bool // ricerca a griglia in corso

	mu       sync.Mutex
	res      Result
	have     bool
	rawPix   []byte // ultimo frame catturato, per l'anteprima durante le ricerche
	rawSeq   uint64
	hist     [histLen]HistPoint
	histN    int
	ctrls    []Control
	camErr   string
	camErrAt time.Time

	// stato dell'elaborazione (solo la sua goroutine)
	lm         headpose.Landmarks
	track      bool
	lastLM     headpose.Landmarks
	lastOK     time.Time
	lastSearch time.Time
	searchGap  time.Duration
	searchMs   float64
	timing     timing
	blink      blinker
}

// NewEngine prepara la pipeline su una webcam aperta; decoders sono le
// goroutine della decodifica MJPEG.
func NewEngine(cam *webcam.Webcam, det *headpose.Detector, focal, fov float64, workers, decoders int, model string) *Engine {
	e := &Engine{Info: cam.Info, cam: cam, det: det, conv: webcam.NewConverter(&cam.Info, decoders), Focal: focal, FOV: fov, Workers: workers,
		MACs: det.MACs(), Model: model, ModelOK: det.Verified,
		free: make(chan *frame, poolFrames), work: make(chan *frame, 1)}
	for i := 0; i < poolFrames; i++ {
		e.free <- &frame{Frame: irimg.Frame{W: cam.Info.W, H: cam.Info.H, Pix: make([]byte, cam.Info.W*cam.Info.H)}}
	}
	e.ctrls = cam.Controls()
	return e
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

// ConvNote descrive la conversione in luminanza.
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
			convErr = e.conv.Gray(data, f.Pix)
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
		f.HostNs = f.captured.UnixNano()
		e.mu.Lock()
		e.rawPix = append(e.rawPix[:0], f.Pix...)
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
			e.handle(f)
			e.free <- f
		}
	}
}

// handle esegue il modello come lo stimatore del driver: se il viso c'era
// nel frame precedente il ritaglio viene dai suoi landmark (un'inferenza),
// altrimenti ricerca a griglia (fino a una trentina di inferenze), sempre
// più di rado finché non trova nulla.
func (e *Engine) handle(f *frame) {
	t0 := time.Now()
	img := &f.Frame
	score := 0.0
	var crop headpose.Crop
	mode := "in attesa della prossima ricerca"
	if e.track && t0.Sub(e.lastOK) < trackWindow {
		crop = headpose.CropFromLandmarks(&e.lastLM)
		score = e.det.Detect(img, crop, &e.lm)
		mode = "inseguimento"
	}
	ran := mode == "inseguimento"
	if score < minFaceScore && t0.Sub(e.lastSearch) >= e.searchGap {
		e.lastSearch = t0
		ts := time.Now()
		e.searching.Store(true)
		crop, score = e.det.Search(img, &e.lm)
		e.searching.Store(false)
		ran = true
		e.searchMs = float64(time.Since(ts).Microseconds()) / 1000
		e.searches.Add(1)
		mode = "ricerca a griglia"
		if score < minFaceScore {
			e.searchGap = min(max(2*e.searchGap, 250*time.Millisecond), 2*time.Second)
		} else {
			e.searchGap = 0
		}
	}
	inferMs := float64(time.Since(t0).Microseconds()) / 1000
	if ran {
		e.timing.add(inferMs)
	}
	face := score >= minFaceScore
	e.track = face
	var m Metrics
	var box *[4]float64
	if face {
		e.lastLM, e.lastOK = e.lm, t0
		e.faces.Add(1)
		m = computeMetrics(&e.lm, img.W, img.H, e.Focal)
		box = &m.Box
	}
	ear := (m.Eyes[0].EAR + m.Eyes[1].EAR) / 2
	prevBlinks := e.blink.total
	e.blink.update(ear, f.captured, face)
	if e.blink.total != prevBlinks {
		e.blinks.Add(1)
	}
	st := imageStats(img.Pix, img.W, img.H, box)
	mean, p95, mx := e.timing.stats()
	e.processed.Add(1)
	now := time.Now()
	e.pipelineNs.Add(now.Sub(f.captured).Nanoseconds())

	e.mu.Lock()
	r := &e.res
	pix := r.Pix
	*r = Result{Seq: f.seq, CamSeq: f.camSeq, W: img.W, H: img.H, Captured: f.captured, Latency: f.latency,
		Processed: now, Bytes: f.bytes, Face: face, Score: score, Mode: mode, Crop: crop, InferMs: inferMs,
		InfMean: mean, InfP95: p95, InfMax: mx, SearchMs: e.searchMs, M: m, Img: st,
		Closed: e.blink.closed && face, BlinkBase: e.blink.base, BlinksMin: len(e.blink.times), BlinkDur: e.blink.lastDur}
	r.Pix = append(pix[:0], img.Pix...)
	if face {
		r.LM = e.lm
	}
	hp := HistPoint{T: f.captured, Ran: ran, Face: face, PoseOK: m.PoseOK, InferMs: inferMs, Score: score, DistMM: m.DistMM}
	if face {
		hp.EAR = [2]float64{m.Eyes[0].EAR, m.Eyes[1].EAR}
		if m.PoseOK {
			hp.Pitch, hp.Yaw, hp.Roll = deg(m.Pose.Pitch), deg(m.Pose.Yaw), deg(m.Pose.Roll)
		}
	}
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
	Res  Result // ultimo frame elaborato (senza pixel: sono in Pix)
	Have bool   // c'è almeno un frame elaborato

	// Immagine dell'anteprima: di norma il frame elaborato (Overlay: i
	// landmark di Res sono i suoi); se il modello è indietro di almeno due
	// frame (ricerca a griglia) l'ultimo catturato, senza sovrapposizioni.
	Pix        []byte
	PixSeq     uint64
	PixW, PixH int
	Overlay    bool
	Searching  bool
	Hist       []HistPoint // dal più vecchio
	C          Counters
	Ctrls      []Control
	Err        string
	ErrSince   time.Time
}

// Snapshot copia lo stato in dst; restituisce true se l'immagine
// dell'anteprima è cambiata. Non torna mai a un frame più vecchio di quello
// già mostrato.
func (e *Engine) Snapshot(dst *Snapshot) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.have {
		dst.Res = e.res
		dst.Res.Pix = nil
	}
	dst.Have = e.have
	live := !e.have || e.rawSeq >= e.res.Seq+2
	seq, src, w, h := e.res.Seq, e.res.Pix, e.res.W, e.res.H
	if live {
		seq, src, w, h = e.rawSeq, e.rawPix, e.Info.W, e.Info.H
	}
	fresh := seq > dst.PixSeq && len(src) == w*h && w > 0
	if fresh {
		dst.Pix = append(dst.Pix[:0], src...)
		dst.PixSeq, dst.PixW, dst.PixH, dst.Overlay = seq, w, h, !live
	}
	dst.Searching = e.searching.Load()
	n := min(e.histN, histLen)
	dst.Hist = dst.Hist[:0]
	for i := e.histN - n; i < e.histN; i++ {
		dst.Hist = append(dst.Hist, e.hist[i%histLen])
	}
	dst.Ctrls = append(dst.Ctrls[:0], e.ctrls...)
	dst.Err, dst.ErrSince = e.camErr, e.camErrAt
	dst.C = Counters{
		Captured: e.captured.Load(), BusyDrops: e.busyDrops.Load(), SeqGaps: e.seqGaps.Load(),
		BufErrors: e.bufErrors.Load(), DecodeErrs: e.decodeErrs.Load(), Bytes: e.bytes.Load(),
		Processed: e.processed.Load(), Faces: e.faces.Load(), Searches: e.searches.Load(), Blinks: e.blinks.Load(),
		DecodeNs: e.decodeNs.Load(), LatencyNs: e.latencyNs.Load(), PipelineNs: e.pipelineNs.Load(),
	}
	return fresh
}
