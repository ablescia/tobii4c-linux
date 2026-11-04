// Package tracker orchestra la sessione col dispositivo e la pipeline
// concorrente di lettura/decodifica:
//
//	           rawCh (buffer minimo)          jobs              results
//	┌────────┐  []byte crudi  ┌────────┐  frame + seq  ┌─────────┐  Sample  ┌─────────┐
//	│ reader │ ─────────────→ │ framer │ ────────────→ │ worker  │ ───────→ │ emitter │ → sink UDP/LSL
//	│ 1 gor. │ ← rawFree      │ 1 gor. │ ← frameFree   │ ×N gor. │          │ riordino│
//	└────────┘                └────────┘               └─────────┘          └─────────┘
//	 bloccata solo su          riassembla, instrada     TLV→XDS→Sample      ordine per seq,
//	 USB IN 0x83               risposte e stream        zero-alloc          drop detection
//
// I buffer crudi e i buffer di frame sono pre-allocati e riciclati tramite
// canali free-list: in regime stazionario il percorso dei dati non alloca.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/gousb"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/usbdev"
	"github.com/ablescia/tobii4c-linux/udev"
)

// Sink riceve i campioni, in ordine di Seq, dalla sola goroutine emitter.
type Sink interface {
	Write(s *gaze.Sample) error
	Close() error
}

// StreamHook riceve i messaggi di stream diversi dal gaze (o tutti, vedi
// Options.HookGaze). Chiamato dalla goroutine framer: payload va copiato se
// conservato e la funzione deve essere veloce.
type StreamHook func(stream uint32, rxNs int64, payload []byte)

// HeadPose stima la rotazione della testa dalle immagini (internal/headpose).
// Se Options.HeadPose non è nil il tracker sottoscrive lo stream immagine
// 0x501 e:
//   - OfferImage riceve ogni immagine dalla goroutine framer (non deve
//     bloccare; il payload va copiato);
//   - Observe riceve ogni campione dalla goroutine emitter, prima dei sink, e
//     può completarne la posa della testa;
//   - SetDevice riceve numero di serie e display area a ogni lettura di
//     quest'ultima.
type HeadPose interface {
	OfferImage(payload []byte, rxNs int64)
	Observe(s *gaze.Sample)
	SetDevice(serial string, area *ttp.DisplayArea)
}

// Options configura il tracker.
type Options struct {
	USB         usbdev.Config
	Workers     int              // goroutine di decodifica (default 2)
	RawQueue    int              // capacità del canale reader→framer (default 1)
	RawBuffers  int              // buffer crudi pre-allocati (default 8)
	FramePool   int              // frame in volo tra framer ed emitter (default 64)
	KeyMap      gaze.KeyMap      // mappa colonne dello stream 0x500
	DisplayArea *ttp.DisplayArea // angoli espliciti da scrivere all'avvio
	DisplaySpec *DisplaySpec     // oppure schermo L×A + offset, calcolato col mounting
	// CalibrationBlob, se presente, viene applicato (op 1110) a ogni connessione.
	CalibrationBlob []byte
	ExtraStreams    []uint32 // stream aggiuntivi da sottoscrivere
	Presence        bool     // sottoscrive 0x504 e ne copia l'ultimo stato in ogni campione; se rifiutato resta PresenceUnknown
	HeadPose        HeadPose // pitch della testa dalle immagini 0x501 (nil = disattivato)
	// ImagesWhenAway: con HeadPose e Presence, lo stream immagine resta attivo
	// anche quando l'utente è assente. Di default viene sospeso dopo
	// imagePauseAfter di assenza: le immagini sono buie e il 4C, mentre le
	// invia, salta circa un frame gaze al minuto.
	ImagesWhenAway bool
	RequestTimeout time.Duration // default 1.5 s
	StallTimeout   time.Duration // nessun frame gaze per questo tempo ⇒ resubscribe/reconnect
	KeepAlive      time.Duration // ping periodico (0 = disattivato)
	StatsInterval  time.Duration // log periodico delle statistiche (0 = disattivato)
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	Hook           StreamHook
	HookGaze       bool // passa anche il gaze all'hook (modalità dump)
	NoSubscribe    bool // solo handshake e interrogazioni (modalità info)
	FailFast       bool // un errore di apertura termina Run invece di riprovare
	OnReady        func(*Session) error
	OnClose        func(*Session)                  // sessione terminata (prima della riconnessione)
	OnNotification func(id uint32, payload []byte) // dalla goroutine framer: veloce, copiare il payload
}

func (o *Options) defaults() {
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.RawQueue < 0 {
		o.RawQueue = 0
	} else if o.RawQueue == 0 {
		o.RawQueue = 1
	}
	if o.RawBuffers <= 0 {
		o.RawBuffers = 8
	}
	if o.FramePool <= 0 {
		o.FramePool = 64
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 1500 * time.Millisecond
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = 3 * time.Second
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = time.Second
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 10 * time.Second
	}
	if o.USB.TransferSize == 0 {
		o.USB = usbdev.DefaultConfig()
	}
	if o.KeyMap == (gaze.KeyMap{}) {
		o.KeyMap = gaze.DefaultKeyMap()
	}
}

// Stats sono contatori cumulativi (lettura atomica, da qualsiasi goroutine).
type Stats struct {
	UsbTransfers   atomic.Uint64
	UsbBytes       atomic.Uint64
	GazeFrames     atomic.Uint64 // frame gaze inoltrati ai sink
	DecodeErrors   atomic.Uint64
	HostDrops      atomic.Uint64 // frame scartati perché la pipeline era satura
	DeviceDrops    atomic.Uint64 // buchi nel frame counter del dispositivo
	DevicePauses   atomic.Uint64 // frame non emessi durante comandi che sospendono lo stream (calibrazione)
	OtherStream    atomic.Uint64
	Notifications  atomic.Uint64
	OrphanReplies  atomic.Uint64
	Resyncs        atomic.Uint64
	SinkErrors     atomic.Uint64
	Reconnects     atomic.Uint64
	LastKeepAliveU atomic.Int64 // RTT ultimo keep-alive, µs
}

// Tracker gestisce connessione, riconnessione e pipeline.
type Tracker struct {
	opt   Options
	log   *slog.Logger
	sinks []Sink
	Stats Stats

	seq uint64 // prossimo Seq da assegnare (solo framer; sessioni non sovrapposte)

	// pauseUntil (unix ns): fino a quest'istante un buco nel frame counter è
	// una sospensione attesa, non una perdita (vedi Session.ExpectPause).
	pauseUntil atomic.Int64
}

// New crea un tracker.
func New(opt Options, log *slog.Logger, sinks ...Sink) *Tracker {
	opt.defaults()
	return &Tracker{opt: opt, log: log, sinks: sinks, seq: 1}
}

// Run si connette e trasmette finché ctx non viene cancellato, riconnettendosi
// con backoff esponenziale su errori o disconnessioni (hotplug).
func (t *Tracker) Run(ctx context.Context) error {
	backoff := t.opt.ReconnectMin
	for {
		err := t.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errOneShotDone) {
			return nil
		}
		var fatal *FatalError
		if errors.As(err, &fatal) {
			return fatal.Err
		}
		if errors.Is(err, gousb.ErrorAccess) {
			// Permessi mancanti: riprovare non serve finché non si installa
			// la regola udev (o si ricollega il dispositivo dopo averla installata).
			return fmt.Errorf("%w\n%s", err, accessHint())
		}
		var openErr *usbdev.OpenError
		if t.opt.FailFast && (errors.As(err, &openErr) || errors.Is(err, usbdev.ErrNotFound)) {
			return err
		}
		lvl := slog.LevelWarn
		if errors.Is(err, usbdev.ErrNotFound) {
			lvl = slog.LevelInfo
		}
		t.log.Log(ctx, lvl, "session ended, reconnecting", "err", err, "in", backoff)
		t.Stats.Reconnects.Add(1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if errors.Is(err, errSessionWasHealthy) {
			backoff = t.opt.ReconnectMin
		} else {
			backoff = min(backoff*2, t.opt.ReconnectMax)
		}
	}
}

// udevRulePath è dove il README installa la regola udev.
var udevRulePath = filepath.Join("/etc/udev/rules.d", udev.FileName)

// accessHint spiega come ottenere l'accesso al nodo USB. Senza regola udev
// propone di installarla; se c'è già, il caso tipico è uaccess senza una
// sessione locale attiva (solo SSH, sistema headless).
func accessHint() string {
	if _, err := os.Stat(udevRulePath); err != nil {
		return fmt.Sprintf("  → installa la regola: %s udev | sudo tee %s && sudo udevadm control --reload-rules && sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104",
			os.Args[0], udevRulePath)
	}
	return "  → la regola " + udevRulePath + " c'è, ma non dà accesso a questo utente.\n" +
		"    Con TAG+=\"uaccess\" serve una sessione locale attiva (desktop o login su console): una sessione SSH non basta.\n" +
		"    Su un sistema headless o usato via SSH abilita la variante plugdev (README, \"Permessi senza root\")."
}

// FatalError interrompe Run senza riconnessione.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }

var (
	errOneShotDone       = errors.New("one-shot session completed")
	errSessionWasHealthy = errors.New("session was streaming")
	errStall             = errors.New("gaze stream stalled")
)

type rawChunk struct {
	buf  []byte // buffer completo da restituire a rawFree
	n    int
	rxNs int64
}

type frameBuf struct{ b []byte }

type job struct {
	seq    uint64
	rxNs   int64
	fb     *frameBuf
	pres   gaze.Presence // stato di presenza all'arrivo del frame
	presUs int64
}

type result struct {
	s  gaze.Sample
	ok bool
	fb *frameBuf // torna al pool solo dopo l'emissione: così i seq in volo
	// sono sempre ≤ FramePool e il ring di riordino non può sovrascriversi.
}

type response struct {
	h       ttp.Header
	payload []byte
}

// Session è una connessione USB attiva.
type Session struct {
	t   *Tracker
	dev *usbdev.Device
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelCauseFunc
	parent context.Context // arresto richiesto dall'utente

	writeMu sync.Mutex
	wbuf    []byte
	seqMu   sync.Mutex
	reqSeq  uint32

	pendingMu sync.Mutex
	pending   map[uint32]chan response

	rawFree   chan []byte
	rawCh     chan rawChunk
	frameFree chan *frameBuf
	jobs      chan job
	results   chan result

	// Presenza corrente: la scrive e la legge solo il framer, che la assegna
	// ai frame gaze nell'ordine in cui arrivano dall'USB.
	presence   gaze.Presence
	presenceUs int64
	presRow    ttp.Row
	presenceCh chan gaze.Presence // framer → supervise, per sospendere lo stream immagine

	images imageGate // stato dello stream immagine (solo goroutine della sessione)

	lastGazeNs atomic.Int64
	subscribed atomic.Bool
	streaming  atomic.Bool // almeno un frame ricevuto
	firstSeq   uint64

	// Info raccolte durante l'handshake.
	infoMu        sync.Mutex
	Mounting      *ttp.Mounting
	DeviceStrings []string
	Catalogue     []ttp.Record
	Columns       map[uint32][]ttp.Record
	Display       *ttp.DisplayArea
}

func (t *Tracker) runSession(parent context.Context) error {
	dev, err := usbdev.Open(t.opt.USB, t.log)
	if err != nil {
		return err
	}
	// Il contesto della sessione NON deriva da parent: alla richiesta di
	// arresto la pipeline deve restare viva quanto basta per l'unsubscribe.
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &Session{
		t: t, dev: dev, log: t.log, ctx: ctx, cancel: cancel, parent: parent,
		wbuf:       make([]byte, 0, 512),
		pending:    map[uint32]chan response{},
		rawFree:    make(chan []byte, t.opt.RawBuffers),
		rawCh:      make(chan rawChunk, t.opt.RawQueue),
		frameFree:  make(chan *frameBuf, t.opt.FramePool),
		jobs:       make(chan job, t.opt.FramePool),
		results:    make(chan result, t.opt.FramePool),
		Columns:    map[uint32][]ttp.Record{},
		presenceCh: make(chan gaze.Presence, 1),
		firstSeq:   t.seq,
	}
	for i := 0; i < t.opt.RawBuffers; i++ {
		s.rawFree <- make([]byte, t.opt.USB.TransferSize)
	}
	for i := 0; i < t.opt.FramePool; i++ {
		s.frameFree <- &frameBuf{b: make([]byte, 0, 4096)}
	}

	var wg, workers sync.WaitGroup
	readerDone := make(chan struct{})
	wg.Add(3)
	go func() { defer wg.Done(); defer close(readerDone); s.reader() }()
	go func() { defer wg.Done(); s.framer() }()
	for i := 0; i < t.opt.Workers; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); s.worker() }()
	}
	go func() { workers.Wait(); close(s.results) }()
	go func() { defer wg.Done(); s.emitter() }()

	err = s.handshake()
	if err == nil && t.opt.OnReady != nil {
		err = t.opt.OnReady(s)
	}
	if err == nil && t.opt.NoSubscribe {
		err = errOneShotDone
	}
	if err == nil {
		err = s.supervise()
	}

	// Arresto ordinato: stop degli stream finché il reader gira ancora.
	if s.subscribed.Load() && ctx.Err() == nil {
		s.stopStreams()
	}
	if t.opt.OnClose != nil {
		t.opt.OnClose(s)
	}
	cancel(err)
	<-readerDone // il reader esce: ReadContext viene cancellato
	wg.Wait()
	dev.Close()
	if s.streaming.Load() && !errors.Is(err, errOneShotDone) {
		err = fmt.Errorf("%w: %v", errSessionWasHealthy, err)
	}
	return err
}

// reader: l'UNICA goroutine che tocca l'endpoint IN. Bloccata nel kernel
// finché una transfer non completa; non fa altro che passare i byte avanti.
func (s *Session) reader() {
	defer close(s.rawCh)
	for {
		var buf []byte
		select {
		case buf = <-s.rawFree:
		case <-s.ctx.Done():
			return
		}
		n, err := s.dev.Read(s.ctx, buf)
		rx := time.Now().UnixNano()
		if err != nil {
			if s.ctx.Err() == nil {
				if usbdev.IsDisconnect(err) {
					s.log.Warn("usb device disconnected", "err", err)
				} else {
					s.log.Error("usb read failed", "err", err)
				}
				s.cancel(fmt.Errorf("usb read: %w", err))
			}
			return
		}
		s.t.Stats.UsbTransfers.Add(1)
		s.t.Stats.UsbBytes.Add(uint64(n))
		select {
		case s.rawCh <- rawChunk{buf: buf, n: n, rxNs: rx}:
		case <-s.ctx.Done():
			return
		}
	}
}

// framer: riassembla e instrada. Unico proprietario del Reassembler.
func (s *Session) framer() {
	defer close(s.jobs)
	r := ttp.NewReassembler(256 * 1024)
	var rx int64
	emit := func(msg []byte) { s.route(msg, rx) }
	var lastResync uint64
	for c := range s.rawCh {
		rx = c.rxNs
		r.Push(c.buf[:c.n], emit)
		s.rawFree <- c.buf
		if r.Resyncs != lastResync {
			s.t.Stats.Resyncs.Add(r.Resyncs - lastResync)
			s.log.Warn("ttp stream resync", "skipped_bytes", r.SkippedBytes, "resyncs", r.Resyncs)
			lastResync = r.Resyncs
		}
	}
}

func (s *Session) route(msg []byte, rx int64) {
	h, payload, err := ttp.ParseHeader(msg)
	if err != nil {
		s.t.Stats.DecodeErrors.Add(1)
		return
	}
	switch h.Marker {
	case ttp.MarkerResponse:
		s.pendingMu.Lock()
		ch, ok := s.pending[h.Seq]
		s.pendingMu.Unlock()
		if !ok {
			s.t.Stats.OrphanReplies.Add(1)
			s.log.Debug("orphan response", "op", hex(h.Op), "seq", h.Seq)
			return
		}
		select {
		case ch <- response{h: h, payload: append([]byte(nil), payload...)}:
		default:
		}

	case ttp.MarkerStream:
		if h.Op != ttp.StreamGaze || s.t.opt.HookGaze {
			if hk := s.t.opt.Hook; hk != nil {
				hk(h.Op, rx, payload)
			}
		}
		if h.Op != ttp.StreamGaze {
			s.t.Stats.OtherStream.Add(1)
			switch {
			case h.Op == ttp.StreamPresence:
				s.updatePresence(payload)
			case h.Op == ttp.StreamImage && s.t.opt.HeadPose != nil:
				s.t.opt.HeadPose.OfferImage(payload, rx)
			}
			return
		}
		s.lastGazeNs.Store(rx)
		s.streaming.Store(true)
		var fb *frameBuf
		select {
		case fb = <-s.frameFree:
		default:
			// Pipeline satura: in hard-realtime si preferisce perdere il
			// frame vecchio piuttosto che accumulare latenza.
			if n := s.t.Stats.HostDrops.Add(1); n == 1 || n%100 == 0 {
				s.log.Warn("frame dropped: pipeline saturated", "host_drops", n)
			}
			return
		}
		fb.b = append(fb.b[:0], payload...)
		seq := s.t.seq
		s.t.seq++
		s.jobs <- job{seq: seq, rxNs: rx, fb: fb, pres: s.presence, presUs: s.presenceUs} // non blocca: cap(jobs) == FramePool

	case ttp.MarkerNotification:
		s.t.Stats.Notifications.Add(1)
		s.log.Info("device notification", "id", h.Op, "values", ttp.ParseU32s(payload))
		if fn := s.t.opt.OnNotification; fn != nil {
			fn(h.Op, payload)
		}

	default:
		s.t.Stats.DecodeErrors.Add(1)
	}
}

// updatePresence aggiorna lo stato di presenza da un messaggio dello stream
// 0x504 (goroutine framer).
func (s *Session) updatePresence(payload []byte) {
	if err := ttp.DecodeRow(payload, &s.presRow); err != nil && s.presRow.N == 0 {
		s.t.Stats.DecodeErrors.Add(1)
		return
	}
	p, us, ok := gaze.DecodePresence(&s.presRow)
	if !ok {
		s.log.Warn("presence message without value", "columns", s.presRow.N)
		return
	}
	if p != s.presence {
		s.log.Info("presence", "state", p.String(), "device_us", us)
		if s.presenceCh != nil {
			select { // l'ultimo stato vince; il framer non aspetta mai
			case <-s.presenceCh:
			default:
			}
			select {
			case s.presenceCh <- p:
			default:
			}
		}
	}
	s.presence, s.presenceUs = p, us
}

// worker: decodifica TLV/XDS → Sample. Ogni worker possiede la sua Row.
func (s *Session) worker() {
	dec := gaze.NewDecoder(s.t.opt.KeyMap)
	var row ttp.Row
	for j := range s.jobs {
		res := result{fb: j.fb}
		res.s.Seq, res.s.HostTimeNs = j.seq, j.rxNs
		res.s.Presence, res.s.PresenceUs = j.pres, j.presUs
		if err := ttp.DecodeRow(j.fb.b, &row); err != nil && row.N == 0 {
			s.t.Stats.DecodeErrors.Add(1)
		} else {
			dec.Decode(&row, &res.s)
			res.ok = true
		}
		s.results <- res
	}
}

// emitter: ripristina l'ordine (i worker finiscono in ordine sparso),
// rileva i frame persi dal dispositivo e scrive sui sink.
func (s *Session) emitter() {
	n := uint64(s.t.opt.FramePool)
	ring := make([]result, n)
	present := make([]bool, n)
	next := s.firstSeq

	var lastCounter uint32
	var counterStep uint32
	haveCounter := false
	var lastDevUs, lastHostNs int64 // campione precedente, per la diagnosi dei buchi

	stats := newLatencyWindow(1024)
	statsEvery := s.t.opt.StatsInterval
	var statsTick <-chan time.Time
	if statsEvery > 0 {
		tk := time.NewTicker(statsEvery)
		defer tk.Stop()
		statsTick = tk.C
	}
	lastFrames := s.t.Stats.GazeFrames.Load()
	lastTick := time.Now()

	for {
		select {
		case r, ok := <-s.results:
			if !ok {
				return
			}
			idx := r.s.Seq % n
			ring[idx], present[idx] = r, true
			for present[next%n] {
				i := next % n
				present[i] = false
				next++
				cur := &ring[i]
				s.frameFree <- cur.fb // mai bloccante: il pool ha cap FramePool
				cur.fb = nil
				if !cur.ok {
					continue
				}
				smp := &cur.s
				// Drop detection sul frame counter del dispositivo.
				if smp.HasCounter {
					if haveCounter {
						d := smp.FrameCounter - lastCounter
						if d > 0 && (counterStep == 0 || d < counterStep) {
							counterStep = d
						}
						if counterStep > 0 && d > counterStep && time.Now().UnixNano() < s.t.pauseUntil.Load() {
							// Il 4C non emette sguardo durante start/clear/compute/scrittura
							// del blob di calibrazione (osservati 15-144 frame): non è una perdita.
							n := uint64(d/counterStep - 1)
							s.t.Stats.DevicePauses.Add(n)
							s.log.Debug("device stream paused by a command", "frames", n)
						} else if counterStep > 0 && d > counterStep {
							lost := uint64(d/counterStep - 1)
							total := s.t.Stats.DeviceDrops.Add(lost)
							// device_gap: salto dell'orologio del tracker tra i due
							// frame ricevuti; host_gap: intervallo di arrivo sul PC. Un
							// host_gap normale (~11 ms) con device_gap doppio vuol dire
							// che il frame non è mai arrivato dall'USB; un host_gap
							// lungo, che la ricezione si è fermata.
							s.log.Warn("device frames lost", "lost", lost, "total", total,
								"counter", smp.FrameCounter, "prev", lastCounter,
								"device_gap_us", smp.DeviceTimeUs-lastDevUs, "host_gap_us", (smp.HostTimeNs-lastHostNs)/1000)
						}
					}
					lastCounter, haveCounter = smp.FrameCounter, true
				}
				lastDevUs, lastHostNs = smp.DeviceTimeUs, smp.HostTimeNs
				if hp := s.t.opt.HeadPose; hp != nil {
					hp.Observe(smp)
				}
				for _, sk := range s.t.sinks {
					if err := sk.Write(smp); err != nil {
						if c := s.t.Stats.SinkErrors.Add(1); c == 1 || c%1000 == 0 {
							s.log.Warn("sink write failed", "err", err, "count", c)
						}
					}
				}
				s.t.Stats.GazeFrames.Add(1)
				stats.add(time.Now().UnixNano() - smp.HostTimeNs)
			}
		case <-statsTick:
			now := time.Now()
			frames := s.t.Stats.GazeFrames.Load()
			hz := float64(frames-lastFrames) / now.Sub(lastTick).Seconds()
			lastFrames, lastTick = frames, now
			p50, p99, max := stats.percentiles()
			s.log.Info("stats",
				"rate_hz", math.Round(hz*10)/10,
				"frames", frames,
				"pipeline_latency_us_p50", p50/1000, "p99", p99/1000, "max", max/1000,
				"host_drops", s.t.Stats.HostDrops.Load(),
				"device_drops", s.t.Stats.DeviceDrops.Load(),
				"device_pauses", s.t.Stats.DevicePauses.Load(),
				"decode_errors", s.t.Stats.DecodeErrors.Load(),
				"resyncs", s.t.Stats.Resyncs.Load(),
				"usb_transfers", s.t.Stats.UsbTransfers.Load(),
				"counter_step", counterStep)
			stats.reset()
		}
	}
}

// latencyWindow raccoglie le latenze senza allocare (buffer fisso).
type latencyWindow struct {
	v []int64
	n int
}

func newLatencyWindow(size int) *latencyWindow { return &latencyWindow{v: make([]int64, size)} }

func (w *latencyWindow) add(ns int64) {
	if w.n < len(w.v) {
		w.v[w.n] = ns
		w.n++
	}
}

func (w *latencyWindow) reset() { w.n = 0 }

func (w *latencyWindow) percentiles() (p50, p99, max int64) {
	if w.n == 0 {
		return 0, 0, 0
	}
	v := w.v[:w.n]
	slices.Sort(v)
	return v[len(v)/2], v[(len(v)*99)/100], v[len(v)-1]
}

func hex(v uint32) string { return fmt.Sprintf("0x%x", v) }
