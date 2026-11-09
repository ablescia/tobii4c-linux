// Package tflite legge i modelli TensorFlow Lite (il formato dei modelli di
// MediaPipe) e li esegue sulla GPU con Vulkan (gpu.go) o, se manca, sulla
// CPU (cpu.go). Gestisce i soli operatori in virgola mobile delle reti di
// MediaPipe Pose: convoluzioni, depthwise, pooling, padding, somme e
// prodotti, concatenazioni, ridimensionamenti bilineari e attivazioni. I pesi
// float16 vengono convertiti in float32 al caricamento.
//
// Layout NHWC come TFLite: un Reshape è solo un cambio di forma.
package tflite

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Tipi dei tensori (schema TFLite).
const (
	TypeFloat32 = 0
	TypeFloat16 = 1
	TypeInt32   = 2
	TypeUint8   = 3
	TypeInt64   = 4
	TypeInt8    = 9
)

// Operatori built-in usati (schema TFLite, enum BuiltinOperator).
const (
	OpAdd            = 0
	OpAveragePool2D  = 1
	OpConcatenation  = 2
	OpConv2D         = 3
	OpDepthwiseConv  = 4
	OpDepthToSpace   = 5
	OpDequantize     = 6
	OpFullyConnected = 9
	OpLogistic       = 14
	OpMaxPool2D      = 17
	OpMul            = 18
	OpRelu           = 19
	OpRelu6          = 21
	OpReshape        = 22
	OpResizeBilinear = 23
	OpTanh           = 28
	OpPad            = 34
	OpTranspose      = 39
	OpMean           = 40
	OpSub            = 41
	OpDiv            = 42
	OpPRelu          = 54
	OpPadV2          = 60
	OpTransposeConv  = 67
	OpResizeNearest  = 97
	OpLeakyRelu      = 98
	OpHardSwish      = 117
	OpDensify        = 124
)

var opNames = map[int]string{
	OpAdd: "ADD", OpAveragePool2D: "AVERAGE_POOL_2D", OpConcatenation: "CONCATENATION", OpConv2D: "CONV_2D",
	OpDepthwiseConv: "DEPTHWISE_CONV_2D", OpDequantize: "DEQUANTIZE", OpFullyConnected: "FULLY_CONNECTED",
	OpLogistic: "LOGISTIC", OpMaxPool2D: "MAX_POOL_2D", OpMul: "MUL", OpRelu: "RELU", OpRelu6: "RELU6",
	OpReshape: "RESHAPE", OpResizeBilinear: "RESIZE_BILINEAR", OpTanh: "TANH", OpPad: "PAD",
	OpTranspose: "TRANSPOSE", OpMean: "MEAN", OpSub: "SUB", OpDiv: "DIV", OpPRelu: "PRELU", OpPadV2: "PADV2",
	OpTransposeConv: "TRANSPOSE_CONV", OpResizeNearest: "RESIZE_NEAREST_NEIGHBOR", OpLeakyRelu: "LEAKY_RELU",
	OpHardSwish: "HARD_SWISH", OpDepthToSpace: "DEPTH_TO_SPACE", OpDensify: "DENSIFY",
}

// OpName è il nome TFLite dell'operatore.
func OpName(code int) string {
	if s, ok := opNames[code]; ok {
		return s
	}
	return fmt.Sprintf("OP_%d", code)
}

// Attivazioni fuse (ActivationFunctionType).
const (
	ActNone  = 0
	ActRelu  = 1
	ActRelu1 = 2
	ActRelu6 = 3
	ActTanh  = 4
)

// Padding delle convoluzioni e dei pooling.
const (
	PadSame  = 0
	PadValid = 1
)

// Tensor è un tensore del grafo; le costanti hanno Data (float) o Ints.
type Tensor struct {
	Name  string
	Shape []int
	Type  int
	Data  []float32 // costante in virgola mobile (float16 già convertite)
	Ints  []int32   // costante intera (forme, padding, assi)
	Const bool
}

// Elems è il numero di elementi.
func (t *Tensor) Elems() int { return elems(t.Shape) }

func elems(s []int) int {
	n := 1
	for _, d := range s {
		n *= d
	}
	return n
}

// Options sono le opzioni degli operatori gestiti.
type Options struct {
	Padding              int
	StrideW, StrideH     int
	DilationW, DilationH int
	DepthMult            int
	Act                  int
	FilterW, FilterH     int
	Axis                 int
	AlignCorners         bool
	HalfPixel            bool
	Alpha                float32 // LEAKY_RELU
	Block                int     // DEPTH_TO_SPACE
}

// Op è un operatore del grafo.
type Op struct {
	Code    int
	Custom  string
	Inputs  []int // -1: ingresso facoltativo assente
	Outputs []int
	Opt     Options
}

// Model è il primo sottografo di un file TFLite.
type Model struct {
	Description string
	Version     int
	Tensors     []Tensor
	Ops         []Op
	Inputs      []int
	Outputs     []int
}

// Parse legge un modello TFLite in memoria. Le costanti vengono copiate:
// data può essere liberato dopo.
func Parse(data []byte) (*Model, error) {
	if len(data) < 8 || string(data[4:8]) != "TFL3" {
		return nil, errors.New("tflite: non è un modello TFLite (manca TFL3)")
	}
	f := &fbuf{b: data}
	root := f.root()
	m := &Model{Version: int(root.u32(0, 0)), Description: root.str(3)}

	codes := root.vec(1)
	opcodes := make([]struct {
		code   int
		custom string
	}, codes.n)
	for k := range opcodes {
		c := codes.table(k)
		code := int(int8(c.u8(0, 0)))
		if b := int(c.i32(3, 0)); b > code {
			code = b
		}
		opcodes[k].code, opcodes[k].custom = code, c.str(1)
	}

	subs := root.vec(2)
	if subs.n < 1 {
		return nil, errors.New("tflite: nessun sottografo")
	}
	sg := subs.table(0)
	bufs := root.vec(4)

	tv := sg.vec(0)
	m.Tensors = make([]Tensor, tv.n)
	for k := range m.Tensors {
		t := tv.table(k)
		tt := &m.Tensors[k]
		tt.Name = t.str(3)
		tt.Shape = t.vec(0).ints()
		tt.Type = int(t.u8(1, 0))
		bi := int(t.u32(2, 0))
		if bi <= 0 || bi >= bufs.n {
			continue
		}
		b := bufs.table(bi)
		raw := b.vec(0).bytes()
		if off := b.u64(1, 0); off > 1 { // modelli con i dati in coda al flatbuffer
			sz := b.u64(2, 0)
			if off+sz > uint64(len(data)) {
				return nil, fmt.Errorf("tflite: tensore %q fuori dal file", tt.Name)
			}
			raw = data[off : off+sz]
		}
		if len(raw) == 0 {
			continue
		}
		if sp := t.sub(6); sp.pos != 0 {
			var err error
			if raw, err = densify(tt, raw, sp); err != nil {
				return nil, err
			}
		}
		if err := tt.load(raw); err != nil {
			return nil, err
		}
	}
	m.Inputs = sg.vec(1).ints()
	m.Outputs = sg.vec(2).ints()

	ov := sg.vec(3)
	m.Ops = make([]Op, ov.n)
	for k := range m.Ops {
		o := ov.table(k)
		ci := int(o.u32(0, 0))
		if ci >= len(opcodes) {
			return nil, fmt.Errorf("tflite: operatore %d: codice %d inesistente", k, ci)
		}
		op := &m.Ops[k]
		op.Code, op.Custom = opcodes[ci].code, opcodes[ci].custom
		op.Inputs = o.vec(1).ints()
		op.Outputs = o.vec(2).ints()
		op.Opt = parseOptions(op.Code, o.sub(4))
	}
	if f.err != nil {
		return nil, f.err
	}
	for _, list := range [][]int{m.Inputs, m.Outputs} {
		for _, i := range list {
			if i < 0 || i >= len(m.Tensors) {
				return nil, fmt.Errorf("tflite: tensore %d inesistente", i)
			}
		}
	}
	for k := range m.Ops {
		for _, list := range [][]int{m.Ops[k].Inputs, m.Ops[k].Outputs} {
			for _, i := range list {
				if i < -1 || i >= len(m.Tensors) {
					return nil, fmt.Errorf("tflite: operatore %d: tensore %d inesistente", k, i)
				}
			}
		}
	}
	return m, nil
}

func (t *Tensor) load(raw []byte) error {
	n := t.Elems()
	switch t.Type {
	case TypeFloat32:
		if len(raw) < 4*n {
			return fmt.Errorf("tflite: tensore %q: %d byte per %d float32", t.Name, len(raw), n)
		}
		t.Data = make([]float32, n)
		for i := range t.Data {
			t.Data[i] = math.Float32frombits(uint32(raw[4*i]) | uint32(raw[4*i+1])<<8 | uint32(raw[4*i+2])<<16 | uint32(raw[4*i+3])<<24)
		}
	case TypeFloat16:
		if len(raw) < 2*n {
			return fmt.Errorf("tflite: tensore %q: %d byte per %d float16", t.Name, len(raw), n)
		}
		t.Data = make([]float32, n)
		for i := range t.Data {
			t.Data[i] = half(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
		}
	case TypeInt32:
		if len(raw) < 4*n {
			return fmt.Errorf("tflite: tensore %q: %d byte per %d int32", t.Name, len(raw), n)
		}
		t.Ints = make([]int32, n)
		for i := range t.Ints {
			t.Ints[i] = int32(uint32(raw[4*i]) | uint32(raw[4*i+1])<<8 | uint32(raw[4*i+2])<<16 | uint32(raw[4*i+3])<<24)
		}
	case TypeInt64:
		if len(raw) < 8*n {
			return fmt.Errorf("tflite: tensore %q: %d byte per %d int64", t.Name, len(raw), n)
		}
		t.Ints = make([]int32, n)
		for i := range t.Ints {
			var v uint64
			for b := 7; b >= 0; b-- {
				v = v<<8 | uint64(raw[8*i+b])
			}
			t.Ints[i] = int32(int64(v))
		}
	default:
		return fmt.Errorf("tflite: tensore %q: tipo %d non gestito (modelli quantizzati?)", t.Name, t.Type)
	}
	t.Const = true
	return nil
}

// densify porta un tensore sparso (SparsityParameters: dimensioni dense o
// CSR, senza blocchi) nella forma densa, con gli zeri espliciti.
func densify(t *Tensor, raw []byte, sp table) ([]byte, error) {
	esz := map[int]int{TypeFloat32: 4, TypeFloat16: 2, TypeInt32: 4, TypeInt8: 1, TypeUint8: 1}[t.Type]
	if esz == 0 {
		return nil, fmt.Errorf("tflite: tensore sparso %q di tipo %d", t.Name, t.Type)
	}
	if sp.vec(1).n > 0 {
		return nil, fmt.Errorf("tflite: tensore sparso %q a blocchi non gestito", t.Name)
	}
	order := sp.vec(0).ints()
	meta := sp.vec(2)
	nd := len(t.Shape)
	if len(order) != nd || meta.n != nd {
		return nil, fmt.Errorf("tflite: tensore sparso %q: metadati di %d dimensioni su %d", t.Name, meta.n, nd)
	}
	type dim struct {
		sparse    bool
		size      int
		seg, idxs []int
	}
	dims := make([]dim, nd)
	for k := range dims {
		m := meta.table(k)
		d := &dims[k]
		d.sparse = m.u8(0, 0) == 1
		d.size = int(m.i32(1, 0))
		if d.sparse {
			d.seg = indexVector(m, 2)
			d.idxs = indexVector(m, 4)
		}
		if order[k] < 0 || order[k] >= nd {
			return nil, fmt.Errorf("tflite: tensore sparso %q: ordine non valido", t.Name)
		}
	}
	// passo di ogni dimensione nel tensore denso
	stride := make([]int, nd)
	s := 1
	for k := nd - 1; k >= 0; k-- {
		stride[k] = s
		s *= t.Shape[k]
	}
	out := make([]byte, s*esz)
	next := 0 // prossimo valore non nullo in raw
	var bad bool
	var walk func(level, prev, off int)
	walk = func(level, prev, off int) {
		if bad {
			return
		}
		if level == nd {
			if (next+1)*esz > len(raw) || off < 0 || (off+1)*esz > len(out) {
				bad = true
				return
			}
			copy(out[off*esz:], raw[next*esz:(next+1)*esz])
			next++
			return
		}
		d := &dims[level]
		ax := order[level]
		if !d.sparse {
			for i := 0; i < d.size; i++ {
				walk(level+1, prev*d.size+i, off+i*stride[ax])
			}
			return
		}
		if prev+1 >= len(d.seg) {
			bad = true
			return
		}
		for j := d.seg[prev]; j < d.seg[prev+1]; j++ {
			if j < 0 || j >= len(d.idxs) {
				bad = true
				return
			}
			walk(level+1, j, off+d.idxs[j]*stride[ax])
		}
	}
	walk(0, 0, 0)
	if bad || sp.f.err != nil {
		return nil, fmt.Errorf("tflite: tensore sparso %q non valido", t.Name)
	}
	return out, nil
}

// indexVector legge un SparseIndexVector (unione: tipo nel campo i, valore
// nel campo i+1): Int32Vector, Uint16Vector o Uint8Vector.
func indexVector(m table, i int) []int {
	typ := m.u8(i, 0)
	v := m.sub(i + 1).vec(0)
	out := make([]int, v.n)
	for k := range out {
		switch typ {
		case 1:
			out[k] = int(v.i32(k))
		case 2:
			out[k] = int(v.f.u16(v.pos + 2*k))
		case 3:
			out[k] = int(v.f.u8(v.pos + k))
		}
	}
	return out
}

// half converte un float16 IEEE in float32.
func half(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	man := uint32(h) & 0x3ff
	switch {
	case exp == 0 && man == 0:
		return math.Float32frombits(sign)
	case exp == 0: // subnormale
		e := uint32(127 - 15 + 1)
		for man&0x400 == 0 {
			man <<= 1
			e--
		}
		man &= 0x3ff
		return math.Float32frombits(sign | e<<23 | man<<13)
	case exp == 0x1f:
		return math.Float32frombits(sign | 0xff<<23 | man<<13)
	}
	return math.Float32frombits(sign | (exp+127-15)<<23 | man<<13)
}

func parseOptions(code int, t table) Options {
	o := Options{StrideW: 1, StrideH: 1, DilationW: 1, DilationH: 1, DepthMult: 1, FilterW: 1, FilterH: 1}
	if t.pos == 0 {
		return o
	}
	switch code {
	case OpConv2D:
		o.Padding = int(t.u8(0, 0))
		o.StrideW, o.StrideH = int(t.i32(1, 1)), int(t.i32(2, 1))
		o.Act = int(t.u8(3, 0))
		o.DilationW, o.DilationH = int(t.i32(4, 1)), int(t.i32(5, 1))
	case OpDepthwiseConv:
		o.Padding = int(t.u8(0, 0))
		o.StrideW, o.StrideH = int(t.i32(1, 1)), int(t.i32(2, 1))
		o.DepthMult = int(t.i32(3, 1))
		o.Act = int(t.u8(4, 0))
		o.DilationW, o.DilationH = int(t.i32(5, 1)), int(t.i32(6, 1))
	case OpTransposeConv:
		o.Padding = int(t.u8(0, 0))
		o.StrideW, o.StrideH = int(t.i32(1, 1)), int(t.i32(2, 1))
		o.Act = int(t.u8(3, 0))
	case OpMaxPool2D, OpAveragePool2D:
		o.Padding = int(t.u8(0, 0))
		o.StrideW, o.StrideH = int(t.i32(1, 1)), int(t.i32(2, 1))
		o.FilterW, o.FilterH = int(t.i32(3, 1)), int(t.i32(4, 1))
		o.Act = int(t.u8(5, 0))
	case OpConcatenation:
		o.Axis = int(t.i32(0, 0))
		o.Act = int(t.u8(1, 0))
	case OpAdd, OpMul, OpSub, OpDiv:
		o.Act = int(t.u8(0, 0))
	case OpFullyConnected:
		o.Act = int(t.u8(0, 0))
	case OpResizeBilinear:
		o.AlignCorners = t.boolean(2)
		o.HalfPixel = t.boolean(3)
	case OpResizeNearest:
		o.AlignCorners = t.boolean(0)
		o.HalfPixel = t.boolean(1)
	case OpDepthToSpace:
		o.Block = int(t.i32(0, 0))
	case OpLeakyRelu:
		if p := t.field(0); p != 0 {
			o.Alpha = math.Float32frombits(t.f.u32(p))
		}
	}
	return o
}

// Summary descrive il modello: ingressi, uscite e operatori per tipo.
func (m *Model) Summary() string {
	var b strings.Builder
	shape := func(i int) string {
		t := &m.Tensors[i]
		return fmt.Sprintf("%s %v", t.Name, t.Shape)
	}
	for _, i := range m.Inputs {
		fmt.Fprintf(&b, "ingresso %s\n", shape(i))
	}
	for _, i := range m.Outputs {
		fmt.Fprintf(&b, "uscita %s\n", shape(i))
	}
	count := map[string]int{}
	for _, op := range m.Ops {
		n := OpName(op.Code)
		if op.Custom != "" {
			n = "CUSTOM:" + op.Custom
		}
		count[n]++
	}
	var names []string
	for n := range count {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%-26s %d\n", n, count[n])
	}
	return b.String()
}
