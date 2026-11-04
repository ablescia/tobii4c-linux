package ttp

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// helloPayload è il payload fisso dell'op 0x3e8 catturato dallo Stream Engine
// Windows (identico in Tropaion/Tobii_Linux e Krazubu/tobii-linux).
var helloPayload = []byte{
	0x00, 0x00, 0x17, 0x00, 0x00, 0x00, 0x28, 0x00, 0x00, 0x00, 0x09, 0x00, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x00, 0x01, 0x00, 0x01, 0x00, 0x02, 0x00, 0x01, 0x00, 0x03, 0x00, 0x01, 0x00, 0x04, 0x00,
	0x01, 0x00, 0x05, 0x00, 0x01, 0x00, 0x06, 0x00, 0x01, 0x00, 0x07, 0x00, 0x01, 0x00, 0x08,
}

// HelloPayload restituisce il payload della richiesta hello.
func HelloPayload() []byte { return helloPayload }

// StreamIDPayload: 00 00 + u32(stream id). Usato da 1210 (colonne) e 1230 (stop).
func StreamIDPayload(id uint32) []byte {
	return AppendU32(PayloadPrefix(make([]byte, 0, 11)), id)
}

// StreamStartPayload: StreamIDPayload + lista vuota 0x17 (come ogni start
// dello Stream Engine).
func StreamStartPayload(id uint32) []byte {
	return AppendEmptyList(StreamIDPayload(id))
}

// DisplayArea: tre angoli in mm nel sistema del tracker (TL, TR, BL); il
// quarto è implicito. +X destra, +Y su (convenzione Tobii).
type DisplayArea struct {
	TopLeft, TopRight, BottomLeft [3]float64
}

// Width e Height in mm.
func (d DisplayArea) Width() float64  { return dist(d.TopLeft, d.TopRight) }
func (d DisplayArea) Height() float64 { return dist(d.TopLeft, d.BottomLeft) }

func dist(a, b [3]float64) float64 {
	var s float64
	for i := range a {
		s += (a[i] - b[i]) * (a[i] - b[i])
	}
	return math.Sqrt(s)
}

func (d DisplayArea) String() string {
	return fmt.Sprintf("TL(%.1f,%.1f,%.1f) TR(%.1f,%.1f,%.1f) BL(%.1f,%.1f,%.1f) = %.0fx%.0f mm",
		d.TopLeft[0], d.TopLeft[1], d.TopLeft[2],
		d.TopRight[0], d.TopRight[1], d.TopRight[2],
		d.BottomLeft[0], d.BottomLeft[1], d.BottomLeft[2], d.Width(), d.Height())
}

// DisplayAreaSetPayload costruisce il payload dell'op 0x5a0 (164 byte):
// 00 00 + 3 × point3d + tag 0x10100 + u32 0x3039 (trailer catturato).
func DisplayAreaSetPayload(d DisplayArea) []byte {
	p := PayloadPrefix(make([]byte, 0, 164))
	p = AppendPoint3D(p, d.TopLeft)
	p = AppendPoint3D(p, d.TopRight)
	p = AppendPoint3D(p, d.BottomLeft)
	p = AppendTag(p, 0x00010100)
	return AppendU32(p, 0x3039)
}

// ParseDisplayArea decodifica la risposta di 0x596 (tre point3d).
func ParseDisplayArea(payload []byte) (DisplayArea, error) {
	r := NewPayloadReader(payload)
	var pts [3][3]float64
	for i := 0; i < 3; i++ {
		var v Value
		if err := readValue(&r, &v); err != nil {
			return DisplayArea{}, err
		}
		if v.Kind != KindPoint || v.N != 3 {
			return DisplayArea{}, errors.New("display area: expected point3d")
		}
		pts[i] = v.F
	}
	return DisplayArea{TopLeft: pts[0], TopRight: pts[1], BottomLeft: pts[2]}, nil
}

// ParseStrings estrae, in ordine, tutte le stringhe 0x14 di primo livello
// (risposta 1420: seriale, modello, generazione, firmware).
func ParseStrings(payload []byte) []string {
	var out []string
	r := NewPayloadReader(payload)
	for {
		t, ok := r.Next()
		if !ok {
			return out
		}
		if t.Type == TypeString {
			if b, ok := t.Counted(); ok {
				out = append(out, string(b))
			}
		}
	}
}

// ParseU32s estrae tutti gli u32 di primo livello.
func ParseU32s(payload []byte) []uint32 {
	var out []uint32
	r := NewPayloadReader(payload)
	for {
		t, ok := r.Next()
		if !ok {
			return out
		}
		if t.Type == TypeU32 || t.Type == TypeU32Alt {
			v, _ := t.U32()
			out = append(out, v)
		}
	}
}

// Record è un elemento di una lista (catalogo stream o colonne di uno stream).
type Record struct {
	ID      uint32   // primo u32 del record: stream id o chiave di colonna
	Name    string   // prima stringa del record
	Strings []string // tutte le stringhe
	U32s    []uint32 // tutti gli u32 (ID compreso)
}

func (r Record) String() string {
	return fmt.Sprintf("0x%04x %q", r.ID, r.Name)
}

// ParseList decodifica una lista di record:
//
//	05 [((n+1)<<16) | 0x0100]   intestazione lista
//	02 [elem]                   type id degli elementi (0x1389 catalogo, 0x1388 colonne)
//	n × ( 05 [(k<<16)|elem] + k TLV )
func ParseList(payload []byte) ([]Record, error) {
	r := NewPayloadReader(payload)
	t, ok := r.Next()
	if !ok || t.Type != TypeTag {
		return nil, errors.New("list: missing header")
	}
	hdr, _ := t.U32()
	if hdr&0xffff != tagTypeList || hdr>>16&0xfff == 0 {
		return nil, fmt.Errorf("list: bad header tag 0x%08x", hdr)
	}
	n := int(hdr>>16&0xfff) - 1
	t, ok = r.Next()
	if !ok || t.Type != TypeU32 {
		return nil, errors.New("list: missing element word")
	}
	out := make([]Record, 0, n)
	for i := 0; i < n; i++ {
		t, ok = r.Next()
		if !ok || t.Type != TypeTag {
			return out, fmt.Errorf("list: record %d: missing prolog", i)
		}
		rtag, _ := t.U32()
		k := int(rtag >> 16 & 0xfff)
		var rec Record
		for j := 0; j < k; j++ {
			f, ok := r.Next()
			if !ok {
				return out, fmt.Errorf("list: record %d truncated", i)
			}
			switch f.Type {
			case TypeU32, TypeU32Alt:
				v, _ := f.U32()
				rec.U32s = append(rec.U32s, v)
			case TypeString:
				if b, ok := f.Counted(); ok {
					rec.Strings = append(rec.Strings, string(b))
				}
			}
		}
		if len(rec.U32s) > 0 {
			rec.ID = rec.U32s[0]
		}
		if len(rec.Strings) > 0 {
			rec.Name = strings.TrimRight(rec.Strings[0], "\x00")
		}
		out = append(out, rec)
	}
	return out, nil
}
