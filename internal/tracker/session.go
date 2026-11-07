package tracker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/usbdev"
)

// ErrTimeout: nessuna risposta entro RequestTimeout.
var ErrTimeout = errors.New("ttp: request timed out")

var errShutdown = errors.New("shutdown requested")

func (s *Session) nextSeq() uint32 {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	s.reqSeq++
	if s.reqSeq == 0 {
		s.reqSeq = 1
	}
	return s.reqSeq
}

// Request invia un comando e attende la risposta con lo stesso seq e op.
// Safe per uso concorrente. Restituisce *ttp.TTPError se il dispositivo rifiuta.
func (s *Session) Request(op uint32, payload []byte) (ttp.Header, []byte, error) {
	return s.RequestTimeout(op, payload, s.t.opt.RequestTimeout)
}

// RequestTimeout è Request con un timeout esplicito (collect, compute e i
// trasferimenti del blob di calibrazione durano secondi).
func (s *Session) RequestTimeout(op uint32, payload []byte, timeout time.Duration) (ttp.Header, []byte, error) {
	seq := s.nextSeq()
	ch := make(chan response, 1)
	s.pendingMu.Lock()
	s.pending[seq] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, seq)
		s.pendingMu.Unlock()
	}()

	if err := s.write(op, seq, payload); err != nil {
		return ttp.Header{}, nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	parentDone := s.parent.Done()
	for {
		select {
		case r := <-ch:
			if r.h.Op != op {
				s.log.Warn("response op mismatch", "want", hex(op), "got", hex(r.h.Op), "seq", seq)
				continue
			}
			if r.h.Error != ttp.ErrNone {
				return r.h, r.payload, &ttp.TTPError{Op: op, Code: r.h.Error}
			}
			return r.h, r.payload, nil
		case <-timer.C:
			return ttp.Header{}, nil, fmt.Errorf("%w (op %s seq %d)", ErrTimeout, hex(op), seq)
		case <-s.ctx.Done():
			return ttp.Header{}, nil, context.Cause(s.ctx)
		case <-parentDone:
			if s.subscribed.Load() {
				parentDone = nil // arresto ordinato: lascia completare l'unsubscribe
				continue
			}
			return ttp.Header{}, nil, errShutdown
		}
	}
}

// write serializza la richiesta. Un messaggio più lungo di MaxOutPiece viene
// spezzato in più bulk OUT, ciascuna col proprio envelope [00 00 00 00][len LE]
// (come lo Stream Engine: pezzi da 4087 byte); il dispositivo risponde una
// volta sola, dopo l'ultimo pezzo.
func (s *Session) write(op, seq uint32, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if ttp.HeaderSize+len(payload) <= ttp.MaxOutPiece {
		s.wbuf = ttp.AppendRequest(s.wbuf[:0], op, seq, payload)
		return s.dev.Write(s.ctx, s.wbuf)
	}
	msg := ttp.AppendRequest(make([]byte, 0, ttp.FullHeader+len(payload)), op, seq, payload)[ttp.EnvelopeSize:]
	piece := make([]byte, 0, ttp.EnvelopeSize+ttp.MaxOutPiece)
	for off := 0; off < len(msg); off += ttp.MaxOutPiece {
		end := min(off+ttp.MaxOutPiece, len(msg))
		piece = append(piece[:0], 0, 0, 0, 0, 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(piece[4:], uint32(end-off))
		piece = append(piece, msg[off:end]...)
		if err := s.dev.Write(s.ctx, piece); err != nil {
			return fmt.Errorf("chunk at %d/%d: %w", off, len(msg), err)
		}
	}
	return nil
}

// handshake porta il dispositivo da "aperto" a "in streaming".
//
// Sequenza (sottoinsieme di quella catturata dallo Stream Engine Windows):
//
//	0x3e8  hello                 obbligatorio
//	0x58c  device strings        informativo (seriale, modello, generazione, firmware)
//	0x640  query realm           informativo (0 = nessuna autenticazione)
//	0x4b0  stream catalogue      informativo (id → nome degli stream del firmware)
//	0x4ba  stream columns        informativo (chiave → nome delle colonne del gaze)
//	0x596  display area get      informativo
//	0x5a0  display area set      solo se configurata
//	0x4c4  subscribe 0x500 (+ 0x504 presence se richiesto, + extra)
func (s *Session) handshake() error {
	l := s.log
	h, p, err := s.Request(ttp.OpHello, ttp.HelloPayload())
	if err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	s.Protocol = ttp.ParseU32s(p)
	l.Info("ttp hello ok", "protocol", fmtU32s(s.Protocol), "status", h.Status)

	if _, p, err := s.Request(ttp.OpDeviceStrings, nil); err == nil {
		s.DeviceStrings = ttp.ParseStrings(p)
		l.Info("device identity", "strings", strings.Join(s.DeviceStrings, " | "))
	} else {
		l.Warn("device strings (0x58c) failed", "err", err)
	}

	if _, p, err := s.Request(ttp.OpQueryRealm, nil); err == nil {
		s.Realm = ttp.ParseU32s(p)
		l.Info("realm query", "values", fmtU32s(s.Realm))
	} else {
		l.Debug("realm query (0x640) failed", "err", err)
	}

	if _, p, err := s.Request(ttp.OpStreamCatalogue, nil); err == nil {
		if recs, err := ttp.ParseList(p); err == nil {
			s.Catalogue = recs
			names := make([]string, 0, len(recs))
			for _, r := range recs {
				names = append(names, r.String())
			}
			l.Info("stream catalogue", "streams", strings.Join(names, ", "))
		} else {
			l.Warn("stream catalogue unparsable", "err", err, "payload_len", len(p))
		}
	} else {
		l.Warn("stream catalogue (0x4b0) failed", "err", err)
	}

	if cols, err := s.StreamColumns(ttp.StreamGaze); err == nil {
		s.checkKeyMap(cols)
	} else {
		l.Warn("gaze columns (0x4ba) failed: keymap non verificabile", "err", err)
	}

	if _, p, err := s.Request(ttp.OpMounting, nil); err == nil {
		if m, err := ttp.ParseMounting(p); err == nil {
			s.Mounting = &m
			l.Info("mounting", "guides", m.Guides, "guide_width_mm", m.WidthMM, "angle_deg", m.AngleDeg)
		} else {
			l.Warn("mounting unparsable", "err", err)
		}
	} else {
		l.Debug("mounting (0x83e) failed", "err", err)
	}

	if d, err := s.ReadDisplayArea(); err == nil {
		l.Info("display area (device)", "area", d.String())
	} else {
		l.Debug("display area get failed", "err", err)
	}
	want := s.t.opt.DisplayArea
	if spec := s.t.opt.DisplaySpec; spec != nil {
		if s.Mounting == nil {
			return &FatalError{Err: errors.New("-display-mm richiede il mounting del dispositivo (op 0x83e), non disponibile")}
		}
		d := ttp.DisplayAreaBasic(spec.WidthMM, spec.HeightMM, spec.OffsetXMM, *s.Mounting)
		want = &d
	}
	if want != nil {
		if err := s.WriteDisplayArea(*want); err != nil {
			l.Error("display area set failed", "err", err)
		} else {
			l.Info("display area set", "area", s.Display.String())
		}
	}
	if blob := s.t.opt.CalibrationBlob; len(blob) > 0 {
		if err := s.WriteCalibration(blob); err != nil {
			l.Error("calibration apply failed", "err", err, "bytes", len(blob))
		} else {
			l.Info("calibration applied", "bytes", len(blob))
		}
	}

	if s.t.opt.NoSubscribe {
		return nil
	}
	return s.subscribeAll()
}

// StreamColumns interroga l'elenco colonne di uno stream (op 0x4ba).
func (s *Session) StreamColumns(stream uint32) ([]ttp.Record, error) {
	_, p, err := s.Request(ttp.OpStreamColumns, ttp.StreamIDPayload(stream))
	if err != nil {
		return nil, err
	}
	recs, err := ttp.ParseList(p)
	if err != nil {
		return nil, err
	}
	s.Columns[stream] = recs
	return recs, nil
}

func (s *Session) subscribeAll() error {
	if _, _, err := s.Request(ttp.OpStreamStart, ttp.StreamStartPayload(ttp.StreamGaze)); err != nil {
		var te *ttp.TTPError
		if errors.As(err, &te) && te.Code == ttp.ErrUnauthorized {
			return &FatalError{Err: fmt.Errorf("subscribe gaze: %w — il firmware richiede l'autenticazione realm, non implementata", err)}
		}
		return fmt.Errorf("subscribe gaze: %w", err)
	}
	s.subscribed.Store(true)
	s.lastGazeNs.Store(time.Now().UnixNano())
	s.log.Info("subscribed", "stream", "0x500 gaze")
	s.infoMu.Lock()
	s.Subscribed = append(s.Subscribed[:0], ttp.StreamGaze)
	s.infoMu.Unlock()
	for _, id := range s.t.secondaryStreams() {
		if _, _, err := s.Request(ttp.OpStreamStart, ttp.StreamStartPayload(id)); err != nil {
			s.log.Warn("subscribe extra stream failed", "stream", hex(id), "err", err)
			s.infoMu.Lock()
			s.SubscribeErrors[id] = err
			s.infoMu.Unlock()
		} else {
			s.log.Info("subscribed", "stream", hex(id))
			s.infoMu.Lock()
			s.Subscribed = append(s.Subscribed, id)
			s.infoMu.Unlock()
			if id == ttp.StreamImage {
				s.images.on = true
			}
		}
	}
	return nil
}

// imagePauseAfter: assenza continua dopo cui lo stream immagine si sospende.
const imagePauseAfter = 3 * time.Second

// imageGate decide quando sospendere e riprendere lo stream immagine: si
// riprende subito appena l'utente non è più assente, si sospende dopo
// imagePauseAfter di assenza continua (la presenza del 4C oscilla spesso per
// qualche centinaio di millisecondi).
type imageGate struct {
	on      bool      // lo stream è sottoscritto
	pauseAt time.Time // istante della sospensione programmata (zero = nessuna)
}

// presence restituisce true se lo stream va ripreso adesso.
func (g *imageGate) presence(p gaze.Presence, now time.Time) (resume bool) {
	if p != gaze.PresenceAway {
		g.pauseAt = time.Time{}
		return !g.on
	}
	if g.on && g.pauseAt.IsZero() {
		g.pauseAt = now.Add(imagePauseAfter)
	}
	return false
}

// due restituisce true se lo stream va sospeso adesso.
func (g *imageGate) due(now time.Time) bool {
	if !g.on || g.pauseAt.IsZero() || now.Before(g.pauseAt) {
		return false
	}
	g.pauseAt = time.Time{}
	return true
}

func (s *Session) imageGating() bool {
	o := &s.t.opt
	return o.HeadPose != nil && o.Presence && !o.ImagesWhenAway
}

// gateImages applica un cambio di presenza (goroutine della sessione: può
// fare richieste al dispositivo).
func (s *Session) gateImages(p gaze.Presence, now time.Time) {
	if !s.imageGating() || !s.images.presence(p, now) {
		return
	}
	if _, _, err := s.Request(ttp.OpStreamStart, ttp.StreamStartPayload(ttp.StreamImage)); err != nil {
		s.log.Warn("image stream resume failed", "err", err)
		return
	}
	s.images.on = true
	s.log.Info("image stream resumed")
}

// pauseImagesIfDue sospende lo stream se l'assenza dura da abbastanza.
func (s *Session) pauseImagesIfDue(now time.Time) {
	if !s.imageGating() || !s.images.due(now) {
		return
	}
	if _, _, err := s.Request(ttp.OpStreamStop, ttp.StreamIDPayload(ttp.StreamImage)); err != nil {
		s.log.Warn("image stream pause failed", "err", err)
		return
	}
	s.images.on = false
	s.log.Info("image stream paused: user away")
}

// secondaryStreams sono gli stream da sottoscrivere dopo il gaze: presence
// (se richiesto) e gli extra, senza duplicati.
func (t *Tracker) secondaryStreams() []uint32 {
	var ids []uint32
	add := func(id uint32) {
		if id != ttp.StreamGaze && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if t.opt.Presence {
		add(ttp.StreamPresence)
	}
	if t.opt.HeadPose != nil {
		add(ttp.StreamImage)
	}
	for _, id := range t.opt.ExtraStreams {
		add(id)
	}
	return ids
}

func (s *Session) stopStreams() {
	ids := append([]uint32{ttp.StreamGaze}, s.t.secondaryStreams()...)
	for _, id := range ids {
		if _, _, err := s.Request(ttp.OpStreamStop, ttp.StreamIDPayload(id)); err != nil {
			s.log.Debug("unsubscribe failed", "stream", hex(id), "err", err)
		}
	}
	s.subscribed.Store(false)
}

// supervise: watchdog sullo stream e keep-alive. Ritorna quando la sessione
// muore (errore USB, stallo non recuperabile) o il contesto viene cancellato.
func (s *Session) supervise() error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var ka <-chan time.Time
	if s.t.opt.KeepAlive > 0 {
		k := time.NewTicker(s.t.opt.KeepAlive)
		defer k.Stop()
		ka = k.C
	}
	stalls := 0
	for {
		select {
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		case <-s.parent.Done():
			return errShutdown
		case <-ka:
			start := time.Now()
			if _, _, err := s.Request(ttp.OpDeviceStrings, nil); err != nil {
				s.log.Warn("keep-alive failed", "err", err)
				if errors.Is(err, ErrTimeout) {
					return fmt.Errorf("keep-alive: %w", err)
				}
			} else {
				rtt := time.Since(start)
				s.t.Stats.LastKeepAliveU.Store(rtt.Microseconds())
				s.log.Debug("keep-alive", "rtt_us", rtt.Microseconds())
			}
		case p := <-s.presenceCh:
			s.gateImages(p, time.Now())
		case now := <-tick.C:
			s.pauseImagesIfDue(now)
			idle := time.Duration(now.UnixNano() - s.lastGazeNs.Load())
			if idle < s.t.opt.StallTimeout {
				if idle < time.Second {
					stalls = 0
				}
				continue
			}
			stalls++
			if stalls > 1 {
				return fmt.Errorf("%w for %s after resubscribe", errStall, idle.Round(time.Millisecond))
			}
			s.log.Warn("gaze stream stalled, resubscribing", "idle", idle.Round(time.Millisecond))
			s.lastGazeNs.Store(now.UnixNano())
			if err := s.subscribeAll(); err != nil {
				return err
			}
		}
	}
}

func fmtU32s(v []uint32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprintf("0x%x", x)
	}
	return strings.Join(parts, ",")
}

// DeviceInfo restituisce i descrittori USB del dispositivo della sessione.
func (s *Session) DeviceInfo() usbdev.Info { return s.dev.Info }

// checkKeyMap confronta la mappa chiavi con le colonne dichiarate dal firmware
// e segnala chiavi mancanti (firmware diverso da quelli verificati).
func (s *Session) checkKeyMap(cols []ttp.Record) {
	names := make(map[uint32]string, len(cols))
	for _, c := range cols {
		names[c.ID] = c.Name
	}
	var missing []string
	for field, key := range s.t.opt.KeyMap.Fields() {
		if key == 0 {
			continue
		}
		if _, ok := names[key]; !ok {
			missing = append(missing, fmt.Sprintf("%s=0x%02x", field, key))
		}
	}
	if len(missing) > 0 {
		s.log.Warn("keymap keys not declared by the device: rivedi con `tobii4c info` e -keymap",
			"missing", strings.Join(missing, ","))
		return
	}
	s.log.Info("gaze columns match keymap", "columns", len(cols),
		"gaze", names[s.t.opt.KeyMap.Gaze], "left_origin", names[s.t.opt.KeyMap.Left.Origin])
}

// DisplaySpec descrive uno schermo per DisplayAreaBasic.
type DisplaySpec struct {
	WidthMM, HeightMM float64
	OffsetXMM         float64 // centro dello schermo a destra del centro del tracker
}

// ReadDisplayArea legge la display area dal dispositivo (op 0x596).
func (s *Session) ReadDisplayArea() (ttp.DisplayArea, error) {
	_, p, err := s.Request(ttp.OpDisplayAreaGet, nil)
	if err != nil {
		return ttp.DisplayArea{}, err
	}
	d, err := ttp.ParseDisplayArea(p)
	if err != nil {
		return d, err
	}
	s.infoMu.Lock()
	s.Display = &d
	s.infoMu.Unlock()
	if hp := s.t.opt.HeadPose; hp != nil {
		hp.SetDevice(s.dev.Info.Serial, &d)
	}
	return d, nil
}

// WriteDisplayArea scrive la display area (op 0x5a0) e la rilegge.
func (s *Session) WriteDisplayArea(d ttp.DisplayArea) error {
	if _, _, err := s.Request(ttp.OpDisplayAreaSet, ttp.DisplayAreaSetPayload(d)); err != nil {
		return err
	}
	_, err := s.ReadDisplayArea()
	return err
}

// Timeout dei comandi di calibrazione: un collect blocca ~0,6-0,9 s mentre il
// dispositivo raccoglie campioni, un compute ~2 s; il blob pesa centinaia di KB.
const (
	CalCommandTimeout = 10 * time.Second
	CalCollectTimeout = 8 * time.Second
	CalComputeTimeout = 30 * time.Second
	CalBlobTimeout    = 30 * time.Second
)

// ReadCalibration restituisce il blob della calibrazione attiva (op 1100).
func (s *Session) ReadCalibration() ([]byte, error) {
	_, p, err := s.RequestTimeout(ttp.OpCalRead, nil, CalBlobTimeout)
	if err != nil {
		return nil, err
	}
	return ttp.CalBlobFromPayload(p)
}

// WriteCalibration applica un blob di calibrazione (op 1110).
func (s *Session) WriteCalibration(blob []byte) error {
	_, _, err := s.RequestTimeout(ttp.OpCalWrite, ttp.CalWritePayload(blob), CalBlobTimeout)
	return err
}

// DisplayInfo restituisce (in modo thread-safe) display area e mounting correnti.
func (s *Session) DisplayInfo() (*ttp.DisplayArea, *ttp.Mounting) {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	var d *ttp.DisplayArea
	if s.Display != nil {
		c := *s.Display
		d = &c
	}
	return d, s.Mounting
}

// ExpectPause dichiara che per i prossimi d lo stream può fermarsi per un
// comando (calibrazione): i buchi nel frame counter in quella finestra non
// vengono contati come frame persi. Ogni chiamata sostituisce la precedente.
func (s *Session) ExpectPause(d time.Duration) {
	s.t.pauseUntil.Store(time.Now().Add(d).UnixNano())
}
