package ttp

import (
	"errors"
	"fmt"
	"strings"
)

// Stream id di diagnostica del firmware (catalogo del 4C, `tobii4c info`).
const (
	StreamAlgoDbg uint32 = 0x1770 // "algodbg": sul 4C righe a 90 Hz con un blob vuoto
	StreamLog     uint32 = 0x1772 // "log": righe di testo del firmware, a eventi
)

// TrackBox è il volume in cui il tracker vede gli occhi, nel sistema del
// tracker (mm). Il firmware lo restituisce come 8 vertici (op 0x578): i primi
// 4 sono il piano vicino, gli altri 4 il piano lontano, ciascuno in ordine
// (+x,+y) (−x,+y) (−x,−y) (+x,−y). Sul 4C: 300×200 mm a 450 mm, 550×400 mm a
// 900 mm.
type TrackBox struct {
	Corners [8][3]float64
}

// Near e Far restituiscono la distanza (z, mm) dei due piani.
func (t TrackBox) Near() float64 { return t.Corners[0][2] }
func (t TrackBox) Far() float64  { return t.Corners[4][2] }

// Size restituisce larghezza e altezza (mm) del piano vicino (far=false) o
// lontano (far=true).
func (t TrackBox) Size(far bool) (w, h float64) {
	o := 0
	if far {
		o = 4
	}
	c := t.Corners[o : o+4]
	return dist(c[0], c[1]), dist(c[1], c[2])
}

func (t TrackBox) String() string {
	nw, nh := t.Size(false)
	fw, fh := t.Size(true)
	return fmt.Sprintf("%.0f×%.0f mm a %.0f mm, %.0f×%.0f mm a %.0f mm", nw, nh, t.Near(), fw, fh, t.Far())
}

// ParseTrackBox decodifica la risposta dell'op 0x578: 8 point3d.
func ParseTrackBox(payload []byte) (TrackBox, error) {
	r := NewPayloadReader(payload)
	var tb TrackBox
	for i := range tb.Corners {
		var v Value
		if err := readValue(&r, &v); err != nil {
			return TrackBox{}, fmt.Errorf("track box corner %d: %w", i, err)
		}
		if v.Kind != KindPoint || v.N != 3 {
			return TrackBox{}, errors.New("track box: expected point3d")
		}
		tb.Corners[i] = v.F
	}
	return tb, nil
}

// Property è una coppia chiave → testo delle risposte di proprietà (op 0x532)
// e di stato (op 0x5d2). Il significato delle chiavi non è documentato: sul 4C
// le proprietà sono, per esempio, 0x0 "Peripheral", 0x2 "HELLO_NIR",
// 0x3 "IS4LTOBIIPERIPHERAL".
type Property struct {
	Key   uint32
	Value string
}

// ParseProperties estrae le coppie (u32, stringa) di una risposta: ogni
// record è 05 [(2<<16)|0x2710] + u32 chiave + stringa.
func ParseProperties(payload []byte) []Property {
	var out []Property
	r := NewPayloadReader(payload)
	var key uint32
	haveKey := false
	for {
		t, ok := r.Next()
		if !ok {
			return out
		}
		switch t.Type {
		case TypeU32, TypeU32Alt:
			key, _ = t.U32()
			haveKey = true
		case TypeString:
			if b, ok := t.Counted(); ok && haveKey {
				out = append(out, Property{Key: key, Value: strings.TrimRight(string(b), "\x00")})
			}
			haveKey = false
		}
	}
}

// LogEntry è una riga del log del firmware (stream 0x1772).
type LogEntry struct {
	DeviceUs int64  // orologio del tracker, lo stesso dello stream gaze
	Text     string // es. "[ 73592.529468] (I) TDI thread: Changed run mode to 22"
}

// Level restituisce la lettera del livello della riga ('I', 'W', 'E', …),
// 0 se la riga non ne ha uno.
func (e LogEntry) Level() byte {
	i := strings.Index(e.Text, "] (")
	if i < 0 || i+4 >= len(e.Text) || e.Text[i+4] != ')' {
		return 0
	}
	return e.Text[i+3]
}

// ParseLogEntries decodifica un messaggio dello stream log. È una riga XDS
// con una sola colonna (0x01 "Log entries") il cui valore è una lista di
// record (05 [(n+1)<<16|0x0100], tipo 0x33f4), ciascuno con un timestamp s64
// e una stringa. Il decoder generico delle righe non scende nelle liste:
// qui si raccolgono le coppie (s64, stringa) nell'ordine in cui compaiono.
func ParseLogEntries(payload []byte) []LogEntry {
	var out []LogEntry
	r := NewPayloadReader(payload)
	var us int64
	haveUs := false
	for {
		t, ok := r.Next()
		if !ok {
			return out
		}
		switch t.Type {
		case TypeS64:
			us, haveUs = t.I64()
		case TypeString:
			if b, ok := t.Counted(); ok && haveUs {
				out = append(out, LogEntry{DeviceUs: us, Text: strings.TrimRight(string(b), "\x00\r\n ")})
			}
			haveUs = false
		}
	}
}
