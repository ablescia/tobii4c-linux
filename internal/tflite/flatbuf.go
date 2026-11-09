package tflite

import (
	"encoding/binary"
	"errors"
)

// Lettura minima dei flatbuffer: tabelle con vtable, vettori, stringhe e
// scalari little-endian. Ogni accesso controlla i limiti del buffer: un file
// corrotto dà un errore, non un panic.

var errBounds = errors.New("tflite: flatbuffer fuori dai limiti")

type fbuf struct {
	b   []byte
	err error
}

func (f *fbuf) fail() {
	if f.err == nil {
		f.err = errBounds
	}
}

func (f *fbuf) u8(p int) byte {
	if p < 0 || p+1 > len(f.b) {
		f.fail()
		return 0
	}
	return f.b[p]
}

func (f *fbuf) u16(p int) uint16 {
	if p < 0 || p+2 > len(f.b) {
		f.fail()
		return 0
	}
	return binary.LittleEndian.Uint16(f.b[p:])
}

func (f *fbuf) u32(p int) uint32 {
	if p < 0 || p+4 > len(f.b) {
		f.fail()
		return 0
	}
	return binary.LittleEndian.Uint32(f.b[p:])
}

func (f *fbuf) u64(p int) uint64 {
	if p < 0 || p+8 > len(f.b) {
		f.fail()
		return 0
	}
	return binary.LittleEndian.Uint64(f.b[p:])
}

// table è la posizione di una tabella (0: assente).
type table struct {
	f   *fbuf
	pos int
}

func (f *fbuf) root() table {
	return table{f, int(f.u32(0))}
}

// field restituisce la posizione del campo i o 0 se manca.
func (t table) field(i int) int {
	if t.pos == 0 || t.f.err != nil {
		return 0
	}
	vt := t.pos - int(int32(t.f.u32(t.pos)))
	vsize := int(t.f.u16(vt))
	o := 4 + 2*i
	if o+2 > vsize {
		return 0
	}
	off := int(t.f.u16(vt + o))
	if off == 0 {
		return 0
	}
	return t.pos + off
}

func (t table) has(i int) bool { return t.field(i) != 0 }

func (t table) i32(i int, def int32) int32 {
	if p := t.field(i); p != 0 {
		return int32(t.f.u32(p))
	}
	return def
}

func (t table) u32(i int, def uint32) uint32 {
	if p := t.field(i); p != 0 {
		return t.f.u32(p)
	}
	return def
}

func (t table) u64(i int, def uint64) uint64 {
	if p := t.field(i); p != 0 {
		return t.f.u64(p)
	}
	return def
}

func (t table) u8(i int, def byte) byte {
	if p := t.field(i); p != 0 {
		return t.f.u8(p)
	}
	return def
}

func (t table) boolean(i int) bool { return t.u8(i, 0) != 0 }

// deref segue un uoffset alla posizione p.
func (f *fbuf) deref(p int) int {
	return p + int(f.u32(p))
}

// sub è la tabella del campo i.
func (t table) sub(i int) table {
	p := t.field(i)
	if p == 0 {
		return table{t.f, 0}
	}
	return table{t.f, t.f.deref(p)}
}

// vector è un vettore: posizione del primo elemento e lunghezza.
type vector struct {
	f   *fbuf
	pos int
	n   int
}

func (t table) vec(i int) vector {
	p := t.field(i)
	if p == 0 {
		return vector{f: t.f}
	}
	v := t.f.deref(p)
	n := int(t.f.u32(v))
	if t.f.err != nil || n < 0 || n > len(t.f.b) {
		t.f.fail()
		return vector{f: t.f}
	}
	// gli elementi si controllano a ogni accesso
	return vector{t.f, v + 4, n}
}

func (v vector) table(k int) table {
	p := v.pos + 4*k
	return table{v.f, v.f.deref(p)}
}

func (v vector) i32(k int) int32 { return int32(v.f.u32(v.pos + 4*k)) }

func (v vector) ints() []int {
	out := make([]int, v.n)
	for k := range out {
		out[k] = int(v.i32(k))
	}
	return out
}

// bytes è il vettore come slice di byte (senza copia).
func (v vector) bytes() []byte {
	if v.n == 0 {
		return nil
	}
	if v.pos < 0 || v.pos+v.n > len(v.f.b) {
		v.f.fail()
		return nil
	}
	return v.f.b[v.pos : v.pos+v.n]
}

func (t table) str(i int) string {
	p := t.field(i)
	if p == 0 {
		return ""
	}
	s := t.f.deref(p)
	n := int(t.f.u32(s))
	if s+4+n > len(t.f.b) || n < 0 {
		t.f.fail()
		return ""
	}
	return string(t.f.b[s+4 : s+4+n])
}
