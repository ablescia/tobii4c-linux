package ttp

import (
	"encoding/binary"
	"math"
)

// Tipi TLV osservati sul filo. Ogni campo è [type u8][len u32 BE][value].
// Il payload di ogni messaggio inizia con due byte 00 00 che vanno saltati.
const (
	TypeU32Alt     uint8 = 0x01 // u32 BE (enum, es. stato di presence)
	TypeU32        uint8 = 0x02 // u32 BE
	TypeFixed16    uint8 = 0x03 // signed 16.16 → i32 / 2^16
	TypeFixed42    uint8 = 0x04 // signed i64 / 2^42 (= 32.32 in 1/1024 mm)
	TypeTag        uint8 = 0x05 // prolog di struttura: tag = (count<<16)|typeID
	TypeS64        uint8 = 0x06 // i64 BE (timestamp µs)
	TypeString     uint8 = 0x14 // u32 BE count + byte ASCII
	TypeBytes      uint8 = 0x15 // u32 BE count + blob
	TypeList       uint8 = 0x17 // u32 BE count + elementi
	TypeFixedList  uint8 = 0x19
	TypeU32Trailer uint8 = 0x1a
)

// Type ID delle strutture (metà bassa del tag).
const (
	tagTypeList      = 0x0100 // lista di record (catalogo, colonne)
	tagTypeRow       = 0x0bb8 // riga XDS (campione di stream)
	tagTypeColumn    = 0x0bb9 // colonna XDS: chiave + valore
	tagTypePoint2D   = 0x1f40
	tagTypePoint3D   = 0x1f41
	tagColumnProloge = 0x00020bb9
)

// Scale dei punti fissi.
const (
	q42Scale = 1 << 42
	q16Scale = 1 << 16
)

// TLV è un campo decodificato; Value punta dentro il buffer originale.
type TLV struct {
	Type  uint8
	Value []byte
}

// U32 interpreta il valore come u32 BE (tipi 0x01, 0x02, 0x05, 0x1a).
func (t TLV) U32() (uint32, bool) {
	if len(t.Value) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(t.Value), true
}

// I64 interpreta il valore come i64 BE (tipi 0x04, 0x06).
func (t TLV) I64() (int64, bool) {
	if len(t.Value) != 8 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(t.Value)), true
}

// Fixed42 decodifica un 0x04 in float64 (mm o coordinate normalizzate).
func (t TLV) Fixed42() (float64, bool) {
	v, ok := t.I64()
	return float64(v) / q42Scale, ok
}

// Fixed16 decodifica un 0x03 16.16 con segno.
func (t TLV) Fixed16() (float64, bool) {
	v, ok := t.U32()
	return float64(int32(v)) / q16Scale, ok
}

// Counted restituisce il corpo di una stringa/blob (0x14, 0x15) senza copia.
func (t TLV) Counted() ([]byte, bool) {
	if len(t.Value) < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(t.Value)
	if uint64(n)+4 > uint64(len(t.Value)) {
		return nil, false
	}
	return t.Value[4 : 4+n], true
}

// Reader itera i TLV di un buffer senza allocare.
type Reader struct {
	buf       []byte
	off       int
	Truncated bool
}

// NewPayloadReader salta il prefisso 00 00 del payload.
func NewPayloadReader(payload []byte) Reader {
	if len(payload) >= 2 {
		payload = payload[2:]
	} else {
		payload = nil
	}
	return Reader{buf: payload}
}

// Next restituisce il prossimo campo; ok=false a fine buffer o su troncamento.
func (r *Reader) Next() (TLV, bool) {
	rest := r.buf[r.off:]
	if len(rest) < 5 {
		if len(rest) != 0 {
			r.Truncated = true
			r.off = len(r.buf)
		}
		return TLV{}, false
	}
	n := binary.BigEndian.Uint32(rest[1:5])
	if uint64(n) > uint64(len(rest)-5) {
		r.Truncated = true
		r.off = len(r.buf)
		return TLV{}, false
	}
	t := TLV{Type: rest[0], Value: rest[5 : 5+n]}
	r.off += 5 + int(n)
	return t, true
}

// Remaining indica se restano byte.
func (r *Reader) Remaining() int { return len(r.buf) - r.off }

// ---------------------------------------------------------------------------
// Writer: costruzione di payload in un buffer fornito dal chiamante.

// PayloadPrefix è l'inizio di ogni payload di richiesta.
func PayloadPrefix(dst []byte) []byte { return append(dst, 0x00, 0x00) }

// AppendU32 aggiunge un TLV 0x02.
func AppendU32(dst []byte, v uint32) []byte {
	dst = append(dst, TypeU32, 0, 0, 0, 4)
	return binary.BigEndian.AppendUint32(dst, v)
}

// AppendTag aggiunge un prolog 0x05.
func AppendTag(dst []byte, tag uint32) []byte {
	dst = append(dst, TypeTag, 0, 0, 0, 4)
	return binary.BigEndian.AppendUint32(dst, tag)
}

// AppendFixed42 aggiunge un TLV 0x04 (arrotondato come lo Stream Engine).
func AppendFixed42(dst []byte, v float64) []byte {
	dst = append(dst, TypeFixed42, 0, 0, 0, 8)
	return binary.BigEndian.AppendUint64(dst, uint64(int64(math.Round(v*q42Scale))))
}

// AppendPoint3D aggiunge un punto 3D: tag 0x00031f41 + 3×Q42 (48 byte).
func AppendPoint3D(dst []byte, p [3]float64) []byte {
	dst = AppendTag(dst, 3<<16|tagTypePoint3D)
	for _, c := range p {
		dst = AppendFixed42(dst, c)
	}
	return dst
}

// AppendEmptyList aggiunge una lista 0x17 vuota.
func AppendEmptyList(dst []byte) []byte {
	return append(dst, TypeList, 0, 0, 0, 4, 0, 0, 0, 0)
}
