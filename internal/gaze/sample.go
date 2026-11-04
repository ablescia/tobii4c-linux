// Package gaze trasforma una riga XDS dello stream 0x500 in un campione
// fortemente tipizzato (Sample) e ne deriva la posa della testa.
//
// Tutte le strutture sono a dimensione fissa e passano per valore: la
// decodifica non tocca l'heap.
package gaze

import (
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// Eye è lo stato di un occhio in un frame.
//
// Il centro della cornea arriva in due sistemi di riferimento, entrambi in mm:
//   - tracker (OriginMM): origine nel tracker, +X verso la destra dell'utente,
//     +Y in alto, +Z verso l'utente, assi della camera;
//   - schermo (OriginDisplayMM): origine al centro della display area, +X
//     lungo il bordo superiore, +Y lungo il bordo sinistro verso l'alto, +Z
//     verso l'utente. È il gaze_origin dello Stream Engine; il firmware lo
//     calcola con la display area configurata (vedi ttp.DisplayArea.ToDisplay).
type Eye struct {
	Tracked bool   // il dispositivo traccia l'occhio (status == 0)
	Status  uint32 // 0 tracciato, 4 non rilevato

	OriginMM           [3]float32 // centro della cornea, sistema del tracker (0x02/0x08)
	OriginValid        bool
	OriginDisplayMM    [3]float32 // centro della cornea, sistema dello schermo (0x22/0x24)
	OriginDisplayValid bool

	Gaze2D      [2]float32 // punto di sguardo dell'occhio, normalizzato sul display
	Gaze2DValid bool       // il firmware non ha una colonna di validità: vale Tracked
	PupilMM     float32
	PupilValid  bool

	// TrackBox è la posizione dell'occhio normalizzata [0,1] nel volume di
	// tracciamento (0x03/0x09, "Eye 3D Relative Position"): è
	// l'eye_position_normalized dello Stream Engine. x cresce verso la
	// sinistra dell'utente, y verso il basso, z allontanandosi dal tracker.
	TrackBox      [3]float32
	TrackBoxValid bool

	// OpennessMM è l'apertura assoluta della palpebra (0x29/0x2b). La validità
	// è quella del firmware e non dipende da Tracked: un occhio chiuso può
	// avere un'apertura valida vicina a zero.
	OpennessMM    float32
	OpennessValid bool
}

// HeadSource indica da dove arriva la posa della testa.
type HeadSource uint8

const (
	HeadNone    HeadSource = iota
	HeadDerived            // ricavata dalla coppia di occhi (questo driver)
	HeadDevice             // stream di head pose del dispositivo
)

// Head è la posa della testa nel sistema dello schermo, come la head_pose
// dello Stream Engine: posizione in mm dal centro della display area, assi
// dello schermo (+X a destra, +Y in alto, +Z verso l'utente).
//
// Rotazioni in radianti, destrorse attorno agli assi dello schermo:
//   - [0] pitch attorno a X: NON osservabile da due soli punti → valid=false
//     se la sorgente è HeadDerived, finché non lo stima il viso nelle
//     immagini IR (internal/headpose);
//   - [1] yaw attorno a Y: positivo = testa ruotata verso la sinistra dell'utente;
//   - [2] roll attorno a Z: inclinazione della linea degli occhi.
//
// DeriveHead ricava yaw e roll dalla linea degli occhi. Sul 4C lo yaw così
// è sottostimato di circa 5 volte (il tracker non distingue bene quale
// occhio è più vicino) e la parte mancante finisce nel roll: con la stima
// dalle immagini attiva, yaw e roll vengono dal viso quando è tracciato.
// RotationImage dice quali angoli vengono dalle immagini.
type Head struct {
	Source        HeadSource
	PositionMM    [3]float32 // punto medio tra gli occhi
	PositionValid bool
	RotationRad   [3]float32
	RotationValid [3]bool
	RotationImage [3]bool // l'angolo viene dal viso nelle immagini IR, non dagli occhi
}

// Presence è lo stato dello stream presence (0x504). I valori sono quelli
// dell'enum tobii_user_presence_status_t dello Stream Engine; sul 4C il
// valore 1 compare quando davanti al sensore non c'è nessuno.
type Presence uint8

const (
	PresenceUnknown Presence = 0
	PresenceAway    Presence = 1
	PresencePresent Presence = 2
)

func (p Presence) String() string {
	switch p {
	case PresenceAway:
		return "away"
	case PresencePresent:
		return "present"
	}
	return "unknown"
}

// Colonne dello stream presence dichiarate dal firmware del 4C (op 0x4ba).
const (
	PresenceKeyTimestamp uint32 = 0x01
	PresenceKeyValue     uint32 = 0x02
)

// DecodePresence legge un messaggio dello stream presence. Lo stream è a
// eventi: il dispositivo lo invia alla sottoscrizione e a ogni cambio di stato.
// Un valore fuori dall'enum diventa PresenceUnknown.
func DecodePresence(row *ttp.Row) (p Presence, deviceUs int64, ok bool) {
	for i := 0; i < row.N; i++ {
		c := &row.Cols[i]
		switch c.Key {
		case PresenceKeyTimestamp:
			if c.Value.Kind == ttp.KindS64 {
				deviceUs = c.Value.I
			}
		case PresenceKeyValue:
			if c.Value.Kind == ttp.KindU32 {
				ok = true
				if c.Value.U <= uint32(PresencePresent) {
					p = Presence(c.Value.U)
				}
			}
		}
	}
	return p, deviceUs, ok
}

// Sample è un campione completo. Value type, 272 byte.
type Sample struct {
	Seq          uint64 // contatore lato host, monotono, senza buchi tra frame inoltrati
	HostTimeNs   int64  // istante di ricezione USB (CLOCK_REALTIME, ns)
	DeviceTimeUs int64  // timestamp del dispositivo (µs, orologio del tracker)
	FrameCounter uint32
	HasCounter   bool

	Gaze         [2]float32 // punto di sguardo combinato (filtrato), normalizzato [0,1]
	GazeValid    bool
	GazeRaw      [2]float32 // combinato non filtrato ("responsive")
	GazeRawValid bool

	Left, Right Eye
	Head        Head

	// Presence è l'ultimo stato dello stream presence ricevuto prima di questo
	// frame (PresenceUnknown finché non ne arriva uno); PresenceUs è il
	// timestamp del dispositivo di quel messaggio, 0 se non ce n'è.
	Presence   Presence
	PresenceUs int64
}

// EyesTrackerMM è il punto medio dei centri corneali nel sistema del tracker,
// per chi lavora con la display area in coordinate del tracker (metriche di
// calibrazione, guida al posizionamento). ok è false se manca un occhio.
func (s *Sample) EyesTrackerMM() (mid [3]float32, ok bool) {
	if !s.Left.OriginValid || !s.Right.OriginValid {
		return mid, false
	}
	for i := range mid {
		mid[i] = (s.Left.OriginMM[i] + s.Right.OriginMM[i]) / 2
	}
	return mid, true
}

// EyeKeys sono le chiavi di colonna per un occhio. 0 = colonna assente.
type EyeKeys struct {
	Status             uint32 `json:"status"`
	Pupil              uint32 `json:"pupil"`
	Origin             uint32 `json:"origin"`
	OriginValid        uint32 `json:"origin_valid"`
	OriginDisplay      uint32 `json:"origin_display"`
	OriginDisplayValid uint32 `json:"origin_display_valid"`
	Gaze2D             uint32 `json:"gaze_2d"`
	Gaze2DValid        uint32 `json:"gaze_2d_valid,omitempty"`
	TrackBox           uint32 `json:"trackbox"`
	TrackBoxValid      uint32 `json:"trackbox_valid"`
	Openness           uint32 `json:"openness"`
	OpennessValid      uint32 `json:"openness_valid"`
}

// KeyMap associa le chiavi XDS ai campi del campione. 0 = campo assente.
type KeyMap struct {
	Timestamp    uint32  `json:"timestamp"`
	FrameCounter uint32  `json:"frame_counter"`
	Gaze         uint32  `json:"gaze"`
	GazeValid    uint32  `json:"gaze_valid"`
	GazeRaw      uint32  `json:"gaze_raw"`
	GazeRawValid uint32  `json:"gaze_raw_valid"`
	Left         EyeKeys `json:"left"`
	Right        EyeKeys `json:"right"`
}

// DefaultKeyMap è la mappa dello stream 0x500.
//
// Le chiavi sono identiche su ET5 (IS5) e 4C (IS4, firmware 4.7.0-d374125):
// sul 4C sono confermate dai nomi che il firmware stesso restituisce per le
// colonne (op 0x4ba, `tobii4c info`), es. 0x1c "Relative Combined Gaze Point",
// 0x02 "Left Gaze Origin Tracker", 0x22 "Left Gaze Origin Display", 0x1d
// "Left Eye 3D Relative Position Validity", 0x29 "Left Absolute Eye
// Openness". Sull'ET5 la decodifica è verificata bit-per-bit contro lo Stream
// Engine Windows (Krazubu/tobii-linux, gaze83.rs). Lo sguardo 2D del singolo
// occhio (0x05/0x0b) non ha una colonna di validità: Gaze2DValid resta 0.
// Sovrascrivibile con LoadKeyMap.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Timestamp: 0x01, FrameCounter: 0x14,
		Gaze: 0x1c, GazeValid: 0x1b,
		GazeRaw: 0x20, GazeRawValid: 0x1f,
		Left: EyeKeys{Status: 0x07, Pupil: 0x06, Origin: 0x02, OriginValid: 0x16,
			OriginDisplay: 0x22, OriginDisplayValid: 0x21, Gaze2D: 0x05,
			TrackBox: 0x03, TrackBoxValid: 0x1d, Openness: 0x29, OpennessValid: 0x2a},
		Right: EyeKeys{Status: 0x0d, Pupil: 0x0c, Origin: 0x08, OriginValid: 0x15,
			OriginDisplay: 0x24, OriginDisplayValid: 0x23, Gaze2D: 0x0b,
			TrackBox: 0x09, TrackBoxValid: 0x1e, Openness: 0x2b, OpennessValid: 0x2c},
	}
}

// Fields restituisce le chiavi per nome di campo (diagnostica).
func (km KeyMap) Fields() map[string]uint32 {
	m := map[string]uint32{
		"timestamp": km.Timestamp, "frame_counter": km.FrameCounter,
		"gaze": km.Gaze, "gaze_valid": km.GazeValid,
		"gaze_raw": km.GazeRaw, "gaze_raw_valid": km.GazeRawValid,
	}
	for side, e := range map[string]EyeKeys{"left": km.Left, "right": km.Right} {
		for f, k := range e.fields() {
			m[side+"."+eyeFieldNames[f]] = k
		}
	}
	return m
}

// fields elenca le chiavi dell'occhio nell'ordine degli slot e*.
func (e EyeKeys) fields() [eyeSlots]uint32 {
	return [eyeSlots]uint32{
		eStatus: e.Status, ePupil: e.Pupil,
		eOrigin: e.Origin, eOriginValid: e.OriginValid,
		eOriginDisplay: e.OriginDisplay, eOriginDisplayValid: e.OriginDisplayValid,
		eGaze2D: e.Gaze2D, eGaze2DValid: e.Gaze2DValid,
		eTrackBox: e.TrackBox, eTrackBoxValid: e.TrackBoxValid,
		eOpenness: e.Openness, eOpennessValid: e.OpennessValid,
	}
}

var eyeFieldNames = [eyeSlots]string{
	eStatus: "status", ePupil: "pupil",
	eOrigin: "origin", eOriginValid: "origin_valid",
	eOriginDisplay: "origin_display", eOriginDisplayValid: "origin_display_valid",
	eGaze2D: "gaze_2d", eGaze2DValid: "gaze_2d_valid",
	eTrackBox: "trackbox", eTrackBoxValid: "trackbox_valid",
	eOpenness: "openness", eOpennessValid: "openness_valid",
}

// LoadKeyMap legge una mappa JSON partendo dai default (i campi omessi restano
// quelli dell'ET5).
func LoadKeyMap(path string) (KeyMap, error) {
	km := DefaultKeyMap()
	b, err := os.ReadFile(path)
	if err != nil {
		return km, err
	}
	if err := json.Unmarshal(b, &km); err != nil {
		return km, fmt.Errorf("keymap %s: %w", path, err)
	}
	return km, nil
}

// slot identifica il campo di destinazione di una chiave.
type slot uint8

const (
	sNone slot = iota
	sTimestamp
	sCounter
	sGaze
	sGazeValid
	sGazeRaw
	sGazeRawValid
	// sEye è il primo slot degli occhi: sEye + lato*eyeSlots + campo e*.
	sEye
)

// Campi di un occhio, nello stesso ordine per sinistro e destro.
const (
	eStatus slot = iota
	ePupil
	eOrigin
	eOriginValid
	eOriginDisplay
	eOriginDisplayValid
	eGaze2D
	eGaze2DValid
	eTrackBox
	eTrackBoxValid
	eOpenness
	eOpennessValid
	eyeSlots
)

// Decoder converte righe XDS in Sample tramite una tabella chiave→slot.
type Decoder struct {
	table    [256]slot
	extended map[uint32]slot // chiavi ≥256 (non osservate su ET5 e 4C)
	// has[lato][campo]: la mappa ha una chiave per quel campo. Senza la
	// colonna di validità ci si affida alle regole di finishEye.
	has [2][eyeSlots]bool
}

// NewDecoder compila la mappa in una lookup table.
func NewDecoder(km KeyMap) *Decoder {
	d := &Decoder{}
	set := func(k uint32, s slot) {
		if k == 0 {
			return
		}
		if k < 256 {
			d.table[k] = s
			return
		}
		if d.extended == nil {
			d.extended = map[uint32]slot{}
		}
		d.extended[k] = s
	}
	set(km.Timestamp, sTimestamp)
	set(km.FrameCounter, sCounter)
	set(km.Gaze, sGaze)
	set(km.GazeValid, sGazeValid)
	set(km.GazeRaw, sGazeRaw)
	set(km.GazeRawValid, sGazeRawValid)
	for side, e := range []EyeKeys{km.Left, km.Right} {
		for f, k := range e.fields() {
			set(k, sEye+slot(side)*eyeSlots+slot(f))
		}
	}
	// Una chiave usata due volte (es. una vecchia mappa con gaze_2d_valid=0x1d,
	// che è la validità del track box) resta all'ultimo campo assegnato: has
	// riflette la tabella finale, non la mappa.
	mark := func(s slot) {
		if s >= sEye {
			d.has[(s-sEye)/eyeSlots][(s-sEye)%eyeSlots] = true
		}
	}
	for _, s := range d.table {
		mark(s)
	}
	for _, s := range d.extended {
		mark(s)
	}
	return d
}

func f2(v ttp.Value) [2]float32         { return [2]float32{float32(v.F[0]), float32(v.F[1])} }
func f3(v ttp.Value) [3]float32         { return [3]float32{float32(v.F[0]), float32(v.F[1]), float32(v.F[2])} }
func isPoint(v ttp.Value, n uint8) bool { return v.Kind == ttp.KindPoint && v.N >= n }
func isScalar(v ttp.Value) bool         { return v.Kind == ttp.KindFixed16 || v.Kind == ttp.KindFixed42 }

// Decode riempie s a partire da row. Non alloca. I campi dell'host (Seq,
// HostTimeNs) e la presenza non vengono toccati: la presenza arriva da un
// altro stream e la imposta chi instrada i messaggi.
func (d *Decoder) Decode(row *ttp.Row, s *Sample) {
	seq, host, pres, presUs := s.Seq, s.HostTimeNs, s.Presence, s.PresenceUs
	*s = Sample{Seq: seq, HostTimeNs: host, Presence: pres, PresenceUs: presUs}
	var statusSeen [2]bool

	for i := 0; i < row.N; i++ {
		c := &row.Cols[i]
		var sl slot
		if c.Key < 256 {
			sl = d.table[c.Key]
		} else if d.extended != nil {
			sl = d.extended[c.Key]
		}
		if sl == sNone {
			continue
		}
		v := c.Value
		if sl >= sEye {
			side := (sl - sEye) / eyeSlots
			e := &s.Left
			if side == 1 {
				e = &s.Right
			}
			switch (sl - sEye) % eyeSlots {
			case eStatus:
				e.Status, e.Tracked = v.U, v.U == 0
				statusSeen[side] = true
			case ePupil:
				if isScalar(v) {
					e.PupilMM = float32(v.F[0])
				}
			case eOrigin:
				if isPoint(v, 3) {
					e.OriginMM = f3(v)
				}
			case eOriginValid:
				e.OriginValid = v.U == 1
			case eOriginDisplay:
				if isPoint(v, 3) {
					e.OriginDisplayMM = f3(v)
				}
			case eOriginDisplayValid:
				e.OriginDisplayValid = v.U == 1
			case eGaze2D:
				if isPoint(v, 2) {
					e.Gaze2D = f2(v)
				}
			case eGaze2DValid:
				e.Gaze2DValid = v.U == 1
			case eTrackBox:
				if isPoint(v, 3) {
					e.TrackBox = f3(v)
				}
			case eTrackBoxValid:
				e.TrackBoxValid = v.U == 1
			case eOpenness:
				if isScalar(v) {
					e.OpennessMM = float32(v.F[0])
				}
			case eOpennessValid:
				e.OpennessValid = v.U == 1
			}
			continue
		}
		switch sl {
		case sTimestamp:
			if v.Kind == ttp.KindS64 {
				s.DeviceTimeUs = v.I
			}
		case sCounter:
			s.FrameCounter, s.HasCounter = v.U, v.Kind == ttp.KindU32
		case sGaze:
			if isPoint(v, 2) {
				s.Gaze = f2(v)
			}
		case sGazeValid:
			s.GazeValid = v.U == 1
		case sGazeRaw:
			if isPoint(v, 2) {
				s.GazeRaw = f2(v)
			}
		case sGazeRawValid:
			s.GazeRawValid = v.U == 1
		}
	}
	// Regola critica (Tropaion, wiki Gaze-Stream): il dispositivo invia i campi
	// dell'occhio anche quando non lo vede, azzerati. La validità si basa sullo
	// status (0 = tracciato), mai sulla sola presenza della colonna.
	d.finishEye(&s.Left, &d.has[0], statusSeen[0])
	d.finishEye(&s.Right, &d.has[1], statusSeen[1])
	DeriveHead(s)
}

func (d *Decoder) finishEye(e *Eye, has *[eyeSlots]bool, statusSeen bool) {
	if !statusSeen {
		// Stream senza colonna di status: ci si fida dei flag di validità.
		e.Tracked = e.OriginValid || e.OriginDisplayValid || e.TrackBoxValid
	}
	// flag vale il flag del firmware se la mappa ne ha la colonna, altrimenti true.
	flag := func(v bool, f slot) bool { return v || !has[f] }
	zero := [3]float32{}
	e.OriginValid = has[eOrigin] && flag(e.OriginValid, eOriginValid) && e.Tracked && e.OriginMM != zero
	e.OriginDisplayValid = has[eOriginDisplay] && flag(e.OriginDisplayValid, eOriginDisplayValid) &&
		e.Tracked && e.OriginDisplayMM != zero
	e.Gaze2DValid = has[eGaze2D] && flag(e.Gaze2DValid, eGaze2DValid) && e.Tracked
	e.TrackBoxValid = has[eTrackBox] && flag(e.TrackBoxValid, eTrackBoxValid) && e.Tracked
	e.PupilValid = e.Tracked && e.PupilMM > 0
	// Il dispositivo segnala l'apertura sconosciuta con −1.
	e.OpennessValid = has[eOpenness] && flag(e.OpennessValid, eOpennessValid) && e.OpennessMM >= 0
}

// DeriveHead ricava posizione, yaw e roll della testa dalla coppia di centri
// corneali nel sistema dello schermo, come la head_pose dello Stream Engine.
// Se il dispositivo ha già fornito una posa (HeadDevice) non fa nulla.
func DeriveHead(s *Sample) {
	if s.Head.Source == HeadDevice {
		return
	}
	l, r := s.Left, s.Right
	if !l.OriginDisplayValid || !r.OriginDisplayValid {
		s.Head = Head{}
		return
	}
	var mid [3]float32
	for i := range mid {
		mid[i] = (l.OriginDisplayMM[i] + r.OriginDisplayMM[i]) / 2
	}
	dx := float64(r.OriginDisplayMM[0] - l.OriginDisplayMM[0])
	dy := float64(r.OriginDisplayMM[1] - l.OriginDisplayMM[1])
	dz := float64(r.OriginDisplayMM[2] - l.OriginDisplayMM[2])
	s.Head = Head{
		Source:        HeadDerived,
		PositionMM:    mid,
		PositionValid: true,
		RotationRad: [3]float32{0,
			float32(math.Atan2(-dz, dx)),
			float32(math.Atan2(dy, dx))},
		RotationValid: [3]bool{false, true, true},
	}
}
