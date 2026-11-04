// Package nn è un interprete minimale di modelli ONNX in Go puro, pensato
// per le piccole reti convolutive di MediaPipe (face landmarker). Supporta
// solo gli operatori che servono a quei grafi: Conv (normale e depthwise),
// PRelu, Add, MaxPool, Pad, Reshape, Transpose, Sigmoid, tutti in float32,
// batch 1.
//
// Il parser legge il formato protobuf di onnx.proto direttamente, senza
// dipendenze: il file del modello è un input esterno e ogni lettura è
// controllata (errore, mai panic).
package nn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Tipi di dato ONNX usati (TensorProto.DataType).
const (
	dtFloat = 1
	dtInt64 = 7
)

var errTrunc = errors.New("onnx: truncated protobuf")

// pbuf è un lettore protobuf con controllo dei limiti.
type pbuf struct {
	b []byte
	i int
}

func (p *pbuf) done() bool { return p.i >= len(p.b) }

func (p *pbuf) varint() (uint64, error) {
	var v uint64
	for s := uint(0); s < 70; s += 7 {
		if p.i >= len(p.b) {
			return 0, errTrunc
		}
		c := p.b[p.i]
		p.i++
		v |= uint64(c&0x7f) << s
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, errors.New("onnx: varint too long")
}

// field legge la chiave del campo successivo.
func (p *pbuf) field() (num int, wire int, err error) {
	k, err := p.varint()
	if err != nil {
		return 0, 0, err
	}
	return int(k >> 3), int(k & 7), nil
}

func (p *pbuf) bytes() ([]byte, error) {
	n, err := p.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(p.b)-p.i) {
		return nil, errTrunc
	}
	b := p.b[p.i : p.i+int(n)]
	p.i += int(n)
	return b, nil
}

func (p *pbuf) fixed32() (uint32, error) {
	if len(p.b)-p.i < 4 {
		return 0, errTrunc
	}
	v := binary.LittleEndian.Uint32(p.b[p.i:])
	p.i += 4
	return v, nil
}

// skip salta un campo del tipo indicato.
func (p *pbuf) skip(wire int) error {
	switch wire {
	case 0:
		_, err := p.varint()
		return err
	case 1:
		if len(p.b)-p.i < 8 {
			return errTrunc
		}
		p.i += 8
	case 2:
		_, err := p.bytes()
		return err
	case 5:
		if len(p.b)-p.i < 4 {
			return errTrunc
		}
		p.i += 4
	default:
		return fmt.Errorf("onnx: unsupported wire type %d", wire)
	}
	return nil
}

// ints legge un campo int64 ripetuto, compatto o no.
func (p *pbuf) ints(wire int, dst []int64) ([]int64, error) {
	if wire == 0 {
		v, err := p.varint()
		return append(dst, int64(v)), err
	}
	if wire != 2 {
		return dst, fmt.Errorf("onnx: bad wire type %d for ints", wire)
	}
	b, err := p.bytes()
	if err != nil {
		return dst, err
	}
	q := pbuf{b: b}
	for !q.done() {
		v, err := q.varint()
		if err != nil {
			return dst, err
		}
		dst = append(dst, int64(v))
	}
	return dst, nil
}

// tensorProto è un initializer del grafo.
type tensorProto struct {
	name  string
	dims  []int
	dtype int
	f32   []float32
	i64   []int64
}

type attribute struct {
	name string
	i    int64
	f    float32
	s    string
	ints []int64
	has  bool
}

type nodeProto struct {
	op     string
	domain string
	in     []string
	out    []string
	attrs  map[string]attribute
}

type graphProto struct {
	nodes   []nodeProto
	inits   map[string]*tensorProto
	inputs  []string
	outputs []string
}

func parseModel(data []byte) (*graphProto, error) {
	p := pbuf{b: data}
	var g *graphProto
	for !p.done() {
		num, wire, err := p.field()
		if err != nil {
			return nil, err
		}
		if num == 7 && wire == 2 {
			b, err := p.bytes()
			if err != nil {
				return nil, err
			}
			if g, err = parseGraph(b); err != nil {
				return nil, err
			}
			continue
		}
		if err := p.skip(wire); err != nil {
			return nil, err
		}
	}
	if g == nil {
		return nil, errors.New("onnx: model without graph")
	}
	return g, nil
}

func parseGraph(b []byte) (*graphProto, error) {
	g := &graphProto{inits: map[string]*tensorProto{}}
	p := pbuf{b: b}
	for !p.done() {
		num, wire, err := p.field()
		if err != nil {
			return nil, err
		}
		if wire != 2 || (num != 1 && num != 5 && num != 11 && num != 12) {
			if err := p.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		sub, err := p.bytes()
		if err != nil {
			return nil, err
		}
		switch num {
		case 1:
			n, err := parseNode(sub)
			if err != nil {
				return nil, err
			}
			g.nodes = append(g.nodes, n)
		case 5:
			t, err := parseTensor(sub)
			if err != nil {
				return nil, err
			}
			g.inits[t.name] = t
		case 11, 12:
			name, err := parseValueInfoName(sub)
			if err != nil {
				return nil, err
			}
			if num == 11 {
				g.inputs = append(g.inputs, name)
			} else {
				g.outputs = append(g.outputs, name)
			}
		}
	}
	return g, nil
}

func parseValueInfoName(b []byte) (string, error) {
	p := pbuf{b: b}
	for !p.done() {
		num, wire, err := p.field()
		if err != nil {
			return "", err
		}
		if num == 1 && wire == 2 {
			s, err := p.bytes()
			return string(s), err
		}
		if err := p.skip(wire); err != nil {
			return "", err
		}
	}
	return "", errors.New("onnx: value info without name")
}

func parseNode(b []byte) (nodeProto, error) {
	n := nodeProto{attrs: map[string]attribute{}}
	p := pbuf{b: b}
	for !p.done() {
		num, wire, err := p.field()
		if err != nil {
			return n, err
		}
		if wire != 2 {
			if err := p.skip(wire); err != nil {
				return n, err
			}
			continue
		}
		s, err := p.bytes()
		if err != nil {
			return n, err
		}
		switch num {
		case 1:
			n.in = append(n.in, string(s))
		case 2:
			n.out = append(n.out, string(s))
		case 4:
			n.op = string(s)
		case 7:
			n.domain = string(s)
		case 5:
			a, err := parseAttr(s)
			if err != nil {
				return n, err
			}
			n.attrs[a.name] = a
		}
	}
	return n, nil
}

func parseAttr(b []byte) (attribute, error) {
	a := attribute{has: true}
	p := pbuf{b: b}
	for !p.done() {
		num, wire, err := p.field()
		if err != nil {
			return a, err
		}
		switch {
		case num == 1 && wire == 2:
			s, err := p.bytes()
			if err != nil {
				return a, err
			}
			a.name = string(s)
		case num == 2 && wire == 5:
			v, err := p.fixed32()
			if err != nil {
				return a, err
			}
			a.f = math.Float32frombits(v)
		case num == 3 && wire == 0:
			v, err := p.varint()
			if err != nil {
				return a, err
			}
			a.i = int64(v)
		case num == 4 && wire == 2:
			s, err := p.bytes()
			if err != nil {
				return a, err
			}
			a.s = string(s)
		case num == 8:
			if a.ints, err = p.ints(wire, a.ints); err != nil {
				return a, err
			}
		default:
			if err := p.skip(wire); err != nil {
				return a, err
			}
		}
	}
	return a, nil
}

func parseTensor(b []byte) (*tensorProto, error) {
	t := &tensorProto{}
	var raw []byte
	var dims, i64 []int64
	var f32 []float32
	p := pbuf{b: b}
	for !p.done() {
		num, wire, err := p.field()
		if err != nil {
			return nil, err
		}
		switch {
		case num == 1:
			if dims, err = p.ints(wire, dims); err != nil {
				return nil, err
			}
		case num == 2 && wire == 0:
			v, err := p.varint()
			if err != nil {
				return nil, err
			}
			t.dtype = int(v)
		case num == 4 && wire == 2: // float_data compatto
			s, err := p.bytes()
			if err != nil {
				return nil, err
			}
			if len(s)%4 != 0 {
				return nil, errors.New("onnx: bad float_data length")
			}
			for j := 0; j < len(s); j += 4 {
				f32 = append(f32, math.Float32frombits(binary.LittleEndian.Uint32(s[j:])))
			}
		case num == 4 && wire == 5:
			v, err := p.fixed32()
			if err != nil {
				return nil, err
			}
			f32 = append(f32, math.Float32frombits(v))
		case num == 7:
			if i64, err = p.ints(wire, i64); err != nil {
				return nil, err
			}
		case num == 8 && wire == 2:
			s, err := p.bytes()
			if err != nil {
				return nil, err
			}
			t.name = string(s)
		case num == 9 && wire == 2:
			if raw, err = p.bytes(); err != nil {
				return nil, err
			}
		case num == 14 && wire == 0:
			v, err := p.varint()
			if err != nil {
				return nil, err
			}
			if v != 0 {
				return nil, fmt.Errorf("onnx: tensor %q uses external data", t.name)
			}
		default:
			if err := p.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	count := 1
	for _, d := range dims {
		if d < 0 || d > 1<<28 {
			return nil, fmt.Errorf("onnx: tensor %q has bad dim %d", t.name, d)
		}
		t.dims = append(t.dims, int(d))
		count *= int(d)
		if count > 1<<28 {
			return nil, fmt.Errorf("onnx: tensor %q too large", t.name)
		}
	}
	switch t.dtype {
	case dtFloat:
		if raw != nil {
			if len(raw) != 4*count {
				return nil, fmt.Errorf("onnx: tensor %q: %d raw bytes for %d floats", t.name, len(raw), count)
			}
			f32 = make([]float32, count)
			for j := range f32 {
				f32[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*j:]))
			}
		}
		if len(f32) != count {
			return nil, fmt.Errorf("onnx: tensor %q: %d floats for %d elements", t.name, len(f32), count)
		}
		t.f32 = f32
	case dtInt64:
		if raw != nil {
			if len(raw) != 8*count {
				return nil, fmt.Errorf("onnx: tensor %q: %d raw bytes for %d int64", t.name, len(raw), count)
			}
			i64 = make([]int64, count)
			for j := range i64 {
				i64[j] = int64(binary.LittleEndian.Uint64(raw[8*j:]))
			}
		}
		if len(i64) != count {
			return nil, fmt.Errorf("onnx: tensor %q: %d int64 for %d elements", t.name, len(i64), count)
		}
		t.i64 = i64
	default:
		// Altri tipi non servono: il tensore resta senza dati e un nodo che
		// lo usa fallisce alla compilazione del piano.
	}
	return t, nil
}
