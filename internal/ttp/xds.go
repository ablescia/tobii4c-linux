package ttp

import (
	"encoding/binary"
	"errors"
	"math"
)

// MaxColumns è il numero massimo di colonne decodificate per riga. L'ET5 ne
// invia 39 nello stream gaze; colonne oltre il limite vengono contate e ignorate.
const MaxColumns = 64

// Kind è il tipo di valore di una colonna XDS.
type Kind uint8

const (
	KindNone Kind = iota
	KindU32
	KindS64
	KindFixed16
	KindFixed42
	KindPoint
	KindBytes
	KindString
	KindUnknown
)

func (k Kind) String() string {
	switch k {
	case KindU32:
		return "u32"
	case KindS64:
		return "s64"
	case KindFixed16:
		return "fixed16"
	case KindFixed42:
		return "fixed42"
	case KindPoint:
		return "point"
	case KindBytes:
		return "bytes"
	case KindString:
		return "string"
	case KindUnknown:
		return "unknown"
	}
	return "none"
}

// Value è il valore tipizzato di una colonna. Value type: niente heap.
type Value struct {
	Kind Kind
	N    uint8      // componenti di un punto (2 o 3)
	U    uint32     // KindU32
	I    int64      // KindS64
	F    [3]float64 // KindFixed16/KindFixed42 in F[0]; KindPoint in F[:N]
	B    []byte     // KindBytes/KindString: vista nel buffer sorgente
}

// Column è una coppia chiave/valore di una riga XDS.
type Column struct {
	Key   uint32
	Value Value
}

// Row è un campione di stream decodificato. Va riusato tra decodifiche: tutti
// i campi sono a dimensione fissa, quindi DecodeRow non alloca.
type Row struct {
	Declared int // colonne annunciate dal tag di riga
	N        int // colonne effettivamente decodificate in Cols
	Overflow int // colonne oltre MaxColumns
	Cols     [MaxColumns]Column
}

// Errori di decodifica XDS.
var (
	ErrNotARow     = errors.New("xds: payload does not start with a row tag")
	ErrBadColumn   = errors.New("xds: malformed column")
	ErrUnsupported = errors.New("xds: unsupported value encoding")
)

// DecodeRow decodifica il payload di un messaggio di stream (0x53) in row.
// Layout:
//
//	00 00
//	05 00000004 [n<<16 | 0x0bb8]            intestazione riga, n colonne
//	ripetuto n volte:
//	  05 00000004 00020bb9                  prolog colonna
//	  02 00000004 [key]                     chiave
//	  <valore>                              TLV scalare, oppure
//	                                        05 [(k<<16)|0x1f4x] + k × 0x04 (punto)
//
// In caso di errore a metà riga restituisce le colonne già decodificate.
func DecodeRow(payload []byte, row *Row) error {
	row.N, row.Overflow, row.Declared = 0, 0, 0
	r := NewPayloadReader(payload)
	t, ok := r.Next()
	if !ok || t.Type != TypeTag {
		return ErrNotARow
	}
	tag, _ := t.U32()
	if tag&0xffff != tagTypeRow {
		return ErrNotARow
	}
	row.Declared = int(tag >> 16 & 0xfff)

	for r.Remaining() > 0 {
		// prolog colonna
		t, ok = r.Next()
		if !ok {
			break
		}
		if t.Type != TypeTag {
			return ErrBadColumn
		}
		ctag, _ := t.U32()
		if ctag&0xffff != tagTypeColumn {
			return ErrBadColumn
		}
		// chiave
		t, ok = r.Next()
		if !ok || (t.Type != TypeU32 && t.Type != TypeU32Alt) {
			return ErrBadColumn
		}
		key, _ := t.U32()
		// valore
		var v Value
		if err := readValue(&r, &v); err != nil {
			return err
		}
		if row.N < MaxColumns {
			row.Cols[row.N] = Column{Key: key, Value: v}
			row.N++
		} else {
			row.Overflow++
		}
	}
	if r.Truncated {
		return ErrBadColumn
	}
	return nil
}

func readValue(r *Reader, v *Value) error {
	t, ok := r.Next()
	if !ok {
		return ErrBadColumn
	}
	switch t.Type {
	case TypeU32, TypeU32Alt, TypeU32Trailer:
		v.Kind = KindU32
		v.U, ok = t.U32()
	case TypeS64:
		v.Kind = KindS64
		v.I, ok = t.I64()
	case TypeFixed16:
		v.Kind = KindFixed16
		v.F[0], ok = t.Fixed16()
	case TypeFixed42:
		v.Kind = KindFixed42
		v.F[0], ok = t.Fixed42()
	case TypeBytes:
		v.Kind = KindBytes
		v.B, ok = t.Counted()
	case TypeString:
		v.Kind = KindString
		v.B, ok = t.Counted()
	case TypeTag:
		tag, _ := t.U32()
		n := int(tag >> 16 & 0xfff)
		switch tag & 0xffff {
		case tagTypePoint2D, tagTypePoint3D:
		default:
			// Struttura sconosciuta: salta i suoi n elementi.
			v.Kind = KindUnknown
			for i := 0; i < n; i++ {
				if _, ok := r.Next(); !ok {
					return ErrBadColumn
				}
			}
			return nil
		}
		v.Kind = KindPoint
		for i := 0; i < n; i++ {
			c, ok := r.Next()
			if !ok {
				return ErrBadColumn
			}
			var f float64
			switch c.Type {
			case TypeFixed42:
				f, ok = c.Fixed42()
			case TypeFixed16:
				f, ok = c.Fixed16()
			default:
				return ErrUnsupported
			}
			if !ok {
				return ErrBadColumn
			}
			if i < len(v.F) {
				v.F[i] = f
			}
		}
		if n > len(v.F) {
			n = len(v.F)
		}
		v.N = uint8(n)
	default:
		v.Kind = KindUnknown
	}
	if !ok {
		return ErrBadColumn
	}
	return nil
}

// Get restituisce la colonna con chiave key (scansione lineare: ≤64 elementi).
func (row *Row) Get(key uint32) (Value, bool) {
	for i := 0; i < row.N; i++ {
		if row.Cols[i].Key == key {
			return row.Cols[i].Value, true
		}
	}
	return Value{}, false
}

// AppendRow codifica row come payload di un messaggio di stream, nello
// stesso formato che DecodeRow legge. Serve al simulatore e ai test.
func AppendRow(dst []byte, row *Row) []byte {
	dst = PayloadPrefix(dst)
	dst = AppendTag(dst, uint32(row.N)<<16|tagTypeRow)
	for i := 0; i < row.N; i++ {
		c := &row.Cols[i]
		dst = AppendTag(dst, tagColumnProloge)
		dst = AppendU32(dst, c.Key)
		dst = appendValue(dst, &c.Value)
	}
	return dst
}

func appendValue(dst []byte, v *Value) []byte {
	counted := func(t uint8, b []byte) []byte {
		dst = append(dst, t)
		dst = binary.BigEndian.AppendUint32(dst, uint32(4+len(b)))
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(b)))
		return append(dst, b...)
	}
	switch v.Kind {
	case KindU32:
		return AppendU32(dst, v.U)
	case KindS64:
		dst = append(dst, TypeS64, 0, 0, 0, 8)
		return binary.BigEndian.AppendUint64(dst, uint64(v.I))
	case KindFixed16:
		dst = append(dst, TypeFixed16, 0, 0, 0, 4)
		return binary.BigEndian.AppendUint32(dst, uint32(int32(math.Round(v.F[0]*q16Scale))))
	case KindFixed42:
		return AppendFixed42(dst, v.F[0])
	case KindPoint:
		typ := uint32(tagTypePoint3D)
		if v.N == 2 {
			typ = tagTypePoint2D
		}
		dst = AppendTag(dst, uint32(v.N)<<16|typ)
		for i := 0; i < int(v.N); i++ {
			dst = AppendFixed42(dst, v.F[i])
		}
		return dst
	case KindBytes:
		return counted(TypeBytes, v.B)
	case KindString:
		return counted(TypeString, v.B)
	}
	return AppendU32(dst, 0)
}
