// Package telemetry è la finestra di telemetria: l'anteprima della camera IR
// con tutti i punti del viso trovati dal modello e, accanto, tutto ciò che il
// driver riceve dal 4C: le colonne dello stream gaze così come le invia il
// firmware, i campioni decodificati, la posa della testa, presenza, immagini,
// log del firmware, identità e configurazione del dispositivo, statistiche
// della pipeline.
//
// Il Collector raccoglie i dati dalla pipeline del tracker senza mai
// bloccarla: un hook nel framer (righe grezze di ogni stream), un sink
// nell'emitter (campioni decodificati) e le callback di sessione. La finestra
// ne copia uno stato coerente a ogni aggiornamento (Snapshot).
package telemetry

import (
	"fmt"
	"sync"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/usbdev"
)

const (
	histLen     = 1024 // campioni per i grafici (~11 s a 90 Hz)
	latLen      = 512  // latenze della pipeline per i percentili
	logLen      = 400  // righe del registro
	presenceLen = 8    // cambi di presenza ricordati
	notesLen    = 8    // notifiche del dispositivo ricordate
)

// HistPoint è un campione ridotto per i grafici nel tempo.
type HistPoint struct {
	HostNs  int64
	Pupil   [2]float32 // mm; sinistro, destro
	PupilOK [2]bool
	Open    [2]float32 // apertura della palpebra, mm
	OpenOK  [2]bool
	Tracked [2]bool
	Gaze    [2]float32
	GazeOK  bool
}

// StreamStat conta i messaggi di uno stream.
type StreamStat struct {
	Count, Bytes uint64
	LastSize     int
	LastRxNs     int64
}

// ImageMeta descrive l'ultimo messaggio dello stream immagine (0x501).
type ImageMeta struct {
	DeviceUs int64
	RxNs     int64
	W, H     int
	BPP      int
	Bytes    int
	Columns  int
	OK       bool
	Err      string
}

// LogLine è una riga del registro: dal firmware (stream log) o dal driver.
type LogLine struct {
	Time     time.Time
	Firmware bool
	Level    byte // 'D', 'I', 'W', 'E'
	Text     string
}

// PresenceEvent è un cambio di stato dello stream presence.
type PresenceEvent struct {
	Time     time.Time
	State    gaze.Presence
	DeviceUs int64
}

// Notification è una notifica asincrona del dispositivo (marker 0x4e).
type Notification struct {
	Time   time.Time
	ID     uint32
	Values []uint32
}

// SessionInfo è ciò che il driver sa del dispositivo dopo l'handshake.
type SessionInfo struct {
	Connected  bool
	Since      time.Time // inizio della sessione corrente
	Sessions   int       // sessioni aperte da quando la finestra è aperta
	ClosedAt   time.Time
	USB        usbdev.Info
	Strings    []string // seriale, modello, generazione, firmware
	Protocol   []uint32
	Realm      []uint32
	Catalogue  []ttp.Record
	Columns    map[uint32][]ttp.Record
	Display    *ttp.DisplayArea
	Mounting   *ttp.Mounting
	TrackBox   *ttp.TrackBox
	Properties []ttp.Property
	Status     []ttp.Property
	OutputRate []uint32
	EnabledEye []uint32
	Subscribed []uint32
	Refused    map[uint32]string
	Probes     map[string]string // errori delle interrogazioni facoltative
}

// rowCopy è una riga XDS copiata fuori dal buffer del framer: i byte delle
// colonne bytes/string vivono in arena, riusata tra una copia e l'altra.
type rowCopy struct {
	row   ttp.Row
	arena []byte
	rxNs  int64
	ok    bool
}

func (rc *rowCopy) set(src *ttp.Row, rxNs int64) {
	rc.row = *src
	rc.arena = rc.arena[:0]
	type span struct{ i, off, n int }
	var spans [ttp.MaxColumns]span
	ns := 0
	for i := 0; i < src.N; i++ {
		if b := src.Cols[i].Value.B; b != nil {
			b = b[:min(len(b), 256)] // basta per la visualizzazione
			spans[ns] = span{i, len(rc.arena), len(b)}
			ns++
			rc.arena = append(rc.arena, b...)
		}
	}
	for _, s := range spans[:ns] {
		rc.row.Cols[s.i].Value.B = rc.arena[s.off : s.off+s.n : s.off+s.n]
	}
	rc.rxNs, rc.ok = rxNs, true
}

// Collector raccoglie i dati della pipeline per la finestra.
type Collector struct {
	// Impostati prima di avviare il tracker.
	Stats  *tracker.Stats
	HP     *headpose.Estimator
	KeyMap gaze.KeyMap

	mu         sync.Mutex
	sample     gaze.Sample
	haveSample bool
	samples    uint64
	hist       [histLen]HistPoint
	histN      uint64
	lat        [latLen]int64
	latN       uint64
	gazeRow    rowCopy
	presRow    rowCopy
	otherRows  map[uint32]*rowCopy
	streams    map[uint32]*StreamStat
	image      ImageMeta
	presence   []PresenceEvent
	logs       [logLen]LogLine
	logN       uint64
	notes      []Notification
	session    SessionInfo
	hpStatus   headpose.Status
	haveHP     bool

	lastCounter, counterStep uint32
	haveCounter              bool

	// solo goroutine framer
	scratch  ttp.Row
	lastPres gaze.Presence
	presSeen bool
}

// New crea un collettore.
func New(km gaze.KeyMap) *Collector {
	return &Collector{KeyMap: km, otherRows: map[uint32]*rowCopy{}, streams: map[uint32]*StreamStat{}}
}

// Write riceve un campione dall'emitter (tracker.Sink). Non alloca.
func (c *Collector) Write(s *gaze.Sample) error {
	now := time.Now().UnixNano()
	c.mu.Lock()
	c.sample, c.haveSample = *s, true
	c.samples++
	h := &c.hist[c.histN%histLen]
	c.histN++
	*h = HistPoint{HostNs: s.HostTimeNs, Gaze: s.Gaze, GazeOK: s.GazeValid}
	for i, e := range [2]*gaze.Eye{&s.Left, &s.Right} {
		h.Pupil[i], h.PupilOK[i] = e.PupilMM, e.PupilValid
		h.Open[i], h.OpenOK[i] = e.OpennessMM, e.OpennessValid
		h.Tracked[i] = e.Tracked
	}
	c.lat[c.latN%latLen] = now - s.HostTimeNs
	c.latN++
	// passo del frame counter: la differenza minima tra frame consecutivi
	if s.HasCounter {
		if c.haveCounter && s.FrameCounter > c.lastCounter {
			if d := s.FrameCounter - c.lastCounter; c.counterStep == 0 || d < c.counterStep {
				c.counterStep = d
			}
		}
		c.lastCounter, c.haveCounter = s.FrameCounter, true
	}
	c.mu.Unlock()
	return nil
}

// Close soddisfa tracker.Sink.
func (c *Collector) Close() error { return nil }

// Hook riceve ogni messaggio di stream dalla goroutine framer
// (tracker.Options.Hook con HookGaze). Deve essere veloce: decodifica la riga
// fuori dal lock e la copia.
func (c *Collector) Hook(stream uint32, rxNs int64, payload []byte) {
	var logs []ttp.LogEntry
	var img ImageMeta
	decoded := false
	switch stream {
	case ttp.StreamLog:
		logs = ttp.ParseLogEntries(payload)
	case ttp.StreamImage:
		img = imageMeta(payload, rxNs, &c.scratch)
	default:
		decoded = ttp.DecodeRow(payload, &c.scratch) == nil || c.scratch.N > 0
	}
	var presEv *PresenceEvent
	if stream == ttp.StreamPresence && decoded {
		if p, us, ok := gaze.DecodePresence(&c.scratch); ok && (!c.presSeen || p != c.lastPres) {
			c.presSeen, c.lastPres = true, p
			presEv = &PresenceEvent{Time: time.Unix(0, rxNs), State: p, DeviceUs: us}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.streams[stream]
	if st == nil {
		st = &StreamStat{}
		c.streams[stream] = st
	}
	st.Count++
	st.Bytes += uint64(len(payload))
	st.LastSize, st.LastRxNs = len(payload), rxNs
	switch stream {
	case ttp.StreamGaze:
		if decoded {
			c.gazeRow.set(&c.scratch, rxNs)
		}
	case ttp.StreamPresence:
		if decoded {
			c.presRow.set(&c.scratch, rxNs)
		}
		if presEv != nil {
			c.presence = append(c.presence, *presEv)
			if len(c.presence) > presenceLen {
				c.presence = c.presence[1:]
			}
		}
	case ttp.StreamImage:
		c.image = img
	case ttp.StreamLog:
		for _, e := range logs {
			lvl := e.Level()
			if lvl == 0 {
				lvl = 'I'
			}
			c.addLogLocked(LogLine{Time: time.Unix(0, rxNs), Firmware: true, Level: lvl, Text: e.Text})
		}
	default:
		if decoded {
			rc := c.otherRows[stream]
			if rc == nil {
				rc = &rowCopy{}
				c.otherRows[stream] = rc
			}
			rc.set(&c.scratch, rxNs)
		}
	}
}

// imageMeta legge le colonne di un'immagine senza copiarne i pixel.
func imageMeta(payload []byte, rxNs int64, row *ttp.Row) ImageMeta {
	m := ImageMeta{RxNs: rxNs}
	if err := ttp.DecodeRow(payload, row); err != nil && row.N == 0 {
		m.Err = err.Error()
		return m
	}
	m.Columns = row.N
	for i := 0; i < row.N; i++ {
		v := row.Cols[i].Value
		switch row.Cols[i].Key {
		case irimg.KeyTimestamp:
			m.DeviceUs = v.I
		case irimg.KeyBPP:
			m.BPP = int(v.U)
		case irimg.KeyWidth:
			m.W = int(v.U)
		case irimg.KeyHeight:
			m.H = int(v.U)
		case irimg.KeyImage:
			m.Bytes = len(v.B)
		}
	}
	m.OK = m.W > 0 && m.H > 0 && m.Bytes == m.W*m.H*max(m.BPP, 8)/8
	return m
}

func (c *Collector) addLogLocked(l LogLine) {
	c.logs[c.logN%logLen] = l
	c.logN++
}

// AddLog aggiunge una riga del driver al registro (vedi LogHandler).
func (c *Collector) AddLog(l LogLine) {
	c.mu.Lock()
	c.addLogLocked(l)
	c.mu.Unlock()
}

// OnNotification registra una notifica del dispositivo (goroutine framer).
func (c *Collector) OnNotification(id uint32, payload []byte) {
	n := Notification{Time: time.Now(), ID: id, Values: ttp.ParseU32s(payload)}
	c.mu.Lock()
	c.notes = append(c.notes, n)
	if len(c.notes) > notesLen {
		c.notes = c.notes[1:]
	}
	c.mu.Unlock()
}

// OnReady raccoglie identità e configurazione del dispositivo all'inizio di
// ogni sessione (tracker.Options.OnReady): colonne di tutti gli stream del
// catalogo e le interrogazioni di sola lettura note (track box, proprietà,
// stato, frequenza, occhi abilitati). Non fallisce mai: un'interrogazione
// rifiutata finisce in Probes.
func (c *Collector) OnReady(s *tracker.Session) error {
	info := SessionInfo{Connected: true, Since: time.Now(), USB: s.DeviceInfo(), Strings: s.DeviceStrings,
		Protocol: s.Protocol, Realm: s.Realm, Catalogue: s.Catalogue, Columns: map[uint32][]ttp.Record{},
		Subscribed: append([]uint32(nil), s.Subscribed...), Refused: map[uint32]string{}, Probes: map[string]string{}}
	for id, err := range s.SubscribeErrors {
		info.Refused[id] = err.Error()
	}
	info.Display, info.Mounting = s.DisplayInfo()
	for id, cols := range s.Columns {
		info.Columns[id] = cols
	}
	for _, r := range s.Catalogue {
		if _, ok := info.Columns[r.ID]; ok {
			continue
		}
		if cols, err := s.StreamColumns(r.ID); err == nil {
			info.Columns[r.ID] = cols
		} else {
			info.Probes[fmt.Sprintf("colonne 0x%x", r.ID)] = err.Error()
		}
	}
	probe := func(name string, op uint32, parse func([]byte) error) {
		_, p, err := s.Request(op, nil)
		if err == nil {
			err = parse(p)
		}
		if err != nil {
			info.Probes[name] = err.Error()
		}
	}
	probe("track box", ttp.OpTrackBox, func(p []byte) error {
		tb, err := ttp.ParseTrackBox(p)
		if err == nil {
			info.TrackBox = &tb
		}
		return err
	})
	probe("proprietà", ttp.OpProperties, func(p []byte) error { info.Properties = ttp.ParseProperties(p); return nil })
	probe("stato", ttp.OpStatus, func(p []byte) error { info.Status = ttp.ParseProperties(p); return nil })
	probe("frequenza", ttp.OpOutputRate, func(p []byte) error { info.OutputRate = ttp.ParseU32s(p); return nil })
	probe("occhi abilitati", ttp.OpGetEnabledEye, func(p []byte) error { info.EnabledEye = ttp.ParseU32s(p); return nil })

	c.mu.Lock()
	info.Sessions = c.session.Sessions + 1
	c.session = info
	c.mu.Unlock()
	return nil
}

// OnClose segna la fine della sessione (tracker.Options.OnClose).
func (c *Collector) OnClose(*tracker.Session) {
	c.mu.Lock()
	c.session.Connected = false
	c.session.ClosedAt = time.Now()
	c.mu.Unlock()
}

// SetHeadPoseStatus registra lo stato dello stimatore (letto fuori dal loop
// dell'interfaccia: Status attende la goroutine del modello).
func (c *Collector) SetHeadPoseStatus(st headpose.Status) {
	c.mu.Lock()
	c.hpStatus, c.haveHP = st, true
	c.mu.Unlock()
}

// SetSession sostituisce le informazioni di sessione (simulatore).
func (c *Collector) SetSession(info SessionInfo) {
	c.mu.Lock()
	c.session = info
	c.mu.Unlock()
}

// Snapshot è una copia coerente dello stato, posseduta dall'interfaccia.
type Snapshot struct {
	Taken      time.Time
	Sample     gaze.Sample
	HaveSample bool
	Samples    uint64
	Hist       []HistPoint // dal più vecchio al più recente
	Latency    []int64     // ns, non ordinate
	GazeRow    ttp.Row
	GazeRowRx  int64
	HaveRow    bool
	PresRow    ttp.Row
	HavePres   bool
	Other      map[uint32]ttp.Row
	Streams    map[uint32]StreamStat
	Image      ImageMeta
	Presence   []PresenceEvent
	Logs       []LogLine // dal più vecchio al più recente
	Notes      []Notification
	Session    SessionInfo
	HPStatus   headpose.Status
	HaveHP     bool
	// CounterStep è la differenza minima del frame counter tra due frame
	// consecutivi (0 finché non è nota).
	CounterStep uint32
}

// Snapshot copia lo stato in dst riusandone la memoria. Le righe grezze
// copiate puntano alle arene del collettore per le colonne bytes/string:
// valgono finché non arriva la riga successiva dello stesso stream, cioè
// vanno solo mostrate, non conservate.
func (c *Collector) Snapshot(dst *Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dst.Taken = time.Now()
	dst.Sample, dst.HaveSample, dst.Samples = c.sample, c.haveSample, c.samples
	n := min(c.histN, histLen)
	dst.Hist = dst.Hist[:0]
	for i := c.histN - n; i < c.histN; i++ {
		dst.Hist = append(dst.Hist, c.hist[i%histLen])
	}
	ln := min(c.latN, latLen)
	dst.Latency = append(dst.Latency[:0], c.lat[:ln]...)
	dst.GazeRow, dst.GazeRowRx, dst.HaveRow = c.gazeRow.row, c.gazeRow.rxNs, c.gazeRow.ok
	dst.PresRow, dst.HavePres = c.presRow.row, c.presRow.ok
	if dst.Other == nil {
		dst.Other = map[uint32]ttp.Row{}
	}
	clear(dst.Other)
	for id, rc := range c.otherRows {
		dst.Other[id] = rc.row
	}
	if dst.Streams == nil {
		dst.Streams = map[uint32]StreamStat{}
	}
	clear(dst.Streams)
	for id, st := range c.streams {
		dst.Streams[id] = *st
	}
	dst.Image = c.image
	dst.Presence = append(dst.Presence[:0], c.presence...)
	lnum := min(c.logN, logLen)
	dst.Logs = dst.Logs[:0]
	for i := c.logN - lnum; i < c.logN; i++ {
		dst.Logs = append(dst.Logs, c.logs[i%logLen])
	}
	dst.Notes = append(dst.Notes[:0], c.notes...)
	dst.Session = c.session
	dst.HPStatus, dst.HaveHP = c.hpStatus, c.haveHP
	dst.CounterStep = c.counterStep
}
