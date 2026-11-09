package tflite

import (
	"fmt"
	"math"
	"sort"
)

// Graph è un modello compilato in una sequenza di nodi su un'arena di
// float32: i pesi in testa, poi i tensori intermedi, che riusano la memoria
// di quelli non più necessari. Lo stesso piano (stessi offset) serve alla
// CPU e alla GPU.
type Graph struct {
	Nodes   []Node
	Values  []Value
	Input   int   // valore d'ingresso
	Outputs []int // valori d'uscita, nell'ordine del modello
	Names   []string

	Arena   int // float dell'arena
	Weights int // float dei pesi (in testa all'arena)
	MACs    int64
	Ops     int // operatori TFLite originali

	released bool
}

// Value è un tensore del piano.
type Value struct {
	Name  string
	Shape []int
	Const bool
	Data  []float32 // costanti, già nel layout del nodo che le usa
	Off   int       // offset nell'arena, in float (-1: nessuno)

	alias      int // RESHAPE: il valore di cui è una vista (-1)
	first      int // nodo che lo produce (-1: ingresso o costante)
	last       int // ultimo nodo che lo legge
	output     bool
	inputValue bool
}

// Elems è il numero di elementi.
func (v *Value) Elems() int { return elems(v.Shape) }

// Kind è il tipo di un nodo.
type Kind int

const (
	KConv      Kind = iota // generica (pesi OHWI)
	KPointwise             // 1×1 a passo 1 (pesi trasposti [Cin][Cout])
	KDepthwise             // pesi [KH][KW][C]
	KPool
	KBinary // somma, prodotto, differenza, quoziente
	KUnary  // attivazione
	KPad
	KResize
	KDepthToSpace
	KConcat
)

var kindNames = [...]string{"conv", "conv 1×1", "depthwise", "pooling", "binaria", "attivazione", "padding",
	"ridimensionamento", "depth-to-space", "concatenazione"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("kind %d", int(k))
}

// Operazioni dei nodi KBinary e attivazioni dei KUnary (oltre ad ActRelu…).
const (
	BinAdd = 0
	BinMul = 1
	BinSub = 2
	BinDiv = 3

	ActLogistic = 5
)

// Node è un'operazione del piano. In e Out sono indici in Values.
type Node struct {
	Kind Kind
	Op   string // operatore TFLite d'origine (per i profili)
	In   []int
	Out  int

	// convoluzioni e pooling
	W, B           int // pesi e bias (-1: nessuno)
	KH, KW, SH, SW int
	DH, DW         int
	PT, PL, PB, PR int
	Act, Act2      int // attivazione del nodo; Act2 dopo la somma residua
	Res            int // valore sommato all'uscita (-1: nessuno)
	Max            bool

	// binarie e unarie
	BinOp int
	NB    int // elementi del secondo operando (broadcast ciclico)

	// padding: inizio di ogni asse (N, H, W, C)
	Pad      [4]int
	PadValue float32

	// ridimensionamento
	HalfPixel    bool
	AlignCorners bool

	Block int // depth-to-space
	Axis  int // concatenazione (asse in NHWC, già positivo)

	MACs int64
}

// Compile prepara il piano di un modello.
func Compile(m *Model) (*Graph, error) {
	if len(m.Inputs) != 1 {
		return nil, fmt.Errorf("tflite: %d ingressi, gestito uno solo", len(m.Inputs))
	}
	g := &Graph{Ops: len(m.Ops)}
	g.Values = make([]Value, len(m.Tensors))
	for i := range m.Tensors {
		t := &m.Tensors[i]
		v := &g.Values[i]
		v.Name, v.Shape, v.alias, v.first, v.last, v.Off = t.Name, t.Shape, -1, -1, -1, -1
		if t.Const {
			v.Const = true
			if t.Data != nil {
				v.Data = t.Data
			} else {
				v.Data = make([]float32, len(t.Ints))
				for k, x := range t.Ints {
					v.Data[k] = float32(x)
				}
			}
		}
	}
	g.Input = m.Inputs[0]
	if s := g.Values[g.Input].Shape; len(s) != 4 || s[0] != 1 {
		return nil, fmt.Errorf("tflite: ingresso %v, atteso [1 H W C]", s)
	}
	g.Values[g.Input].inputValue = true
	ints := func(i int) []int32 { return m.Tensors[i].Ints }

	for k := range m.Ops {
		op := &m.Ops[k]
		name := OpName(op.Code)
		if op.Custom != "" {
			return nil, fmt.Errorf("tflite: operatore %d: CUSTOM %q non gestito", k, op.Custom)
		}
		in := op.Inputs
		if len(op.Outputs) != 1 {
			return nil, fmt.Errorf("tflite: operatore %d (%s): %d uscite", k, name, len(op.Outputs))
		}
		out := op.Outputs[0]
		ov := &g.Values[out]
		arg := func(j int) *Value {
			if j >= len(in) || in[j] < 0 {
				return nil
			}
			return &g.Values[in[j]]
		}
		fail := func(f string, a ...any) error {
			return fmt.Errorf("tflite: operatore %d (%s): %s", k, name, fmt.Sprintf(f, a...))
		}
		switch op.Code {
		case OpDequantize, OpDensify:
			x := arg(0)
			if x == nil || !x.Const {
				return nil, fail("ingresso non costante")
			}
			ov.Const, ov.Data = true, x.Data
			continue
		case OpReshape:
			x := arg(0)
			if x == nil {
				return nil, fail("senza ingresso")
			}
			if x.Elems() != ov.Elems() {
				return nil, fail("forma %v → %v", x.Shape, ov.Shape)
			}
			if x.Const {
				ov.Const, ov.Data = true, x.Data
			} else {
				ov.alias = in[0]
			}
			continue
		}
		n := Node{Op: name, Out: out, W: -1, B: -1, Res: -1, DH: 1, DW: 1, KH: 1, KW: 1, SH: 1, SW: 1, Act: op.Opt.Act}
		for _, j := range in {
			if j >= 0 && !g.Values[j].Const {
				n.In = append(n.In, j)
			}
		}
		switch op.Code {
		case OpConv2D, OpDepthwiseConv:
			x, w := arg(0), arg(1)
			if x == nil || w == nil || !w.Const || len(x.Shape) != 4 || len(w.Shape) != 4 || len(ov.Shape) != 4 {
				return nil, fail("ingressi non validi")
			}
			if len(n.In) != 1 {
				return nil, fail("ingresso non costante mancante")
			}
			n.Kind = KConv
			if op.Code == OpDepthwiseConv {
				n.Kind = KDepthwise
				if op.Opt.DepthMult != 1 || w.Shape[3] != x.Shape[3] {
					return nil, fail("moltiplicatore di profondità %d non gestito", op.Opt.DepthMult)
				}
			} else if w.Shape[3] != x.Shape[3] {
				return nil, fail("canali %d nei pesi, %d nell'ingresso", w.Shape[3], x.Shape[3])
			}
			n.W = in[1]
			if b := arg(2); b != nil {
				if !b.Const {
					return nil, fail("bias non costante")
				}
				n.B = in[2]
			}
			n.KH, n.KW = w.Shape[1], w.Shape[2]
			n.SH, n.SW = op.Opt.StrideH, op.Opt.StrideW
			n.DH, n.DW = op.Opt.DilationH, op.Opt.DilationW
			var err error
			if n.PT, n.PB, err = padding(op.Opt.Padding, x.Shape[1], ov.Shape[1], n.KH, n.SH, n.DH); err != nil {
				return nil, fail("%v", err)
			}
			if n.PL, n.PR, err = padding(op.Opt.Padding, x.Shape[2], ov.Shape[2], n.KW, n.SW, n.DW); err != nil {
				return nil, fail("%v", err)
			}
		case OpMaxPool2D, OpAveragePool2D:
			x := arg(0)
			if x == nil || len(x.Shape) != 4 {
				return nil, fail("ingresso non valido")
			}
			n.Kind, n.Max = KPool, op.Code == OpMaxPool2D
			n.KH, n.KW = op.Opt.FilterH, op.Opt.FilterW
			n.SH, n.SW = op.Opt.StrideH, op.Opt.StrideW
			var err error
			if n.PT, n.PB, err = padding(op.Opt.Padding, x.Shape[1], ov.Shape[1], n.KH, n.SH, 1); err != nil {
				return nil, fail("%v", err)
			}
			if n.PL, n.PR, err = padding(op.Opt.Padding, x.Shape[2], ov.Shape[2], n.KW, n.SW, 1); err != nil {
				return nil, fail("%v", err)
			}
		case OpAdd, OpMul, OpSub, OpDiv:
			a, b := arg(0), arg(1)
			if a == nil || b == nil {
				return nil, fail("ingressi mancanti")
			}
			n.Kind = KBinary
			n.BinOp = map[int]int{OpAdd: BinAdd, OpMul: BinMul, OpSub: BinSub, OpDiv: BinDiv}[op.Code]
			// il primo operando ha la forma dell'uscita, il secondo si ripete
			if a.Elems() != ov.Elems() {
				if op.Code == OpSub || op.Code == OpDiv || b.Elems() != ov.Elems() {
					return nil, fail("broadcast %v %v non gestito", a.Shape, b.Shape)
				}
				in = []int{in[1], in[0]}
				a, b = b, a
			}
			if nb := b.Elems(); nb == 0 || ov.Elems()%nb != 0 || !suffixBroadcast(b.Shape, ov.Shape) {
				return nil, fail("broadcast %v %v non gestito", a.Shape, b.Shape)
			}
			n.In = []int{in[0], in[1]}
			n.NB = b.Elems()
		case OpLogistic, OpRelu, OpRelu6, OpTanh:
			n.Kind = KUnary
			n.Act = map[int]int{OpLogistic: ActLogistic, OpRelu: ActRelu, OpRelu6: ActRelu6, OpTanh: ActTanh}[op.Code]
		case OpPad, OpPadV2:
			x := arg(0)
			if x == nil || len(x.Shape) != 4 || len(in) < 2 || in[1] < 0 || len(ints(in[1])) != 8 {
				return nil, fail("padding non costante o non 4D")
			}
			p := ints(in[1])
			if p[0] != 0 || p[1] != 0 {
				return nil, fail("padding del batch")
			}
			n.Kind = KPad
			for a := 0; a < 4; a++ {
				n.Pad[a] = int(p[2*a])
				if p[2*a] < 0 || p[2*a+1] < 0 || x.Shape[a]+int(p[2*a]+p[2*a+1]) != ov.Shape[a] {
					return nil, fail("forma %v → %v", x.Shape, ov.Shape)
				}
			}
			if op.Code == OpPadV2 && len(in) > 2 && in[2] >= 0 {
				if c := &g.Values[in[2]]; c.Const && len(c.Data) == 1 {
					n.PadValue = c.Data[0]
				} else {
					return nil, fail("valore di padding non costante")
				}
			}
		case OpResizeBilinear:
			x := arg(0)
			if x == nil || len(x.Shape) != 4 || len(ov.Shape) != 4 {
				return nil, fail("ingresso non valido")
			}
			n.Kind, n.HalfPixel, n.AlignCorners = KResize, op.Opt.HalfPixel, op.Opt.AlignCorners
		case OpDepthToSpace:
			x := arg(0)
			b := op.Opt.Block
			if x == nil || len(x.Shape) != 4 || b < 1 || x.Shape[3]%(b*b) != 0 {
				return nil, fail("blocco %d non valido", b)
			}
			n.Kind, n.Block = KDepthToSpace, b
		case OpConcatenation:
			if len(n.In) != len(in) {
				return nil, fail("ingressi costanti non gestiti")
			}
			n.Kind = KConcat
			ax := op.Opt.Axis
			if ax < 0 {
				ax += len(ov.Shape)
			}
			if ax < 0 || ax >= len(ov.Shape) {
				return nil, fail("asse %d", op.Opt.Axis)
			}
			n.Axis = ax
		default:
			return nil, fail("non gestito")
		}
		if len(n.In) == 0 {
			return nil, fail("nessun ingresso variabile")
		}
		g.Nodes = append(g.Nodes, n)
	}
	for _, o := range m.Outputs {
		v := &g.Values[o]
		if v.Const {
			return nil, fmt.Errorf("tflite: uscita %q costante", v.Name)
		}
		g.Outputs = append(g.Outputs, o)
		g.Names = append(g.Names, v.Name)
	}
	g.fusePad()
	g.fuseResidual()
	g.prepareWeights()
	g.allocate()
	return g, nil
}

// padding calcola il padding prima e dopo su un asse come TFLite.
func padding(mode, in, out, k, s, d int) (int, int, error) {
	ek := (k-1)*d + 1
	switch mode {
	case PadSame:
		if want := (in + s - 1) / s; want != out {
			return 0, 0, fmt.Errorf("uscita %d, attesa %d (SAME)", out, want)
		}
		total := max((out-1)*s+ek-in, 0)
		return total / 2, total - total/2, nil
	case PadValid:
		if want := (in-ek)/s + 1; want != out {
			return 0, 0, fmt.Errorf("uscita %d, attesa %d (VALID)", out, want)
		}
		return 0, 0, nil
	}
	return 0, 0, fmt.Errorf("padding %d sconosciuto", mode)
}

// suffixBroadcast dice se b si ripete ciclicamente lungo out (b uguale alle
// ultime dimensioni di out, a meno di dimensioni 1 in testa).
func suffixBroadcast(b, out []int) bool {
	for len(b) > 0 && b[0] == 1 {
		b = b[1:]
	}
	if len(b) > len(out) {
		return false
	}
	tail := out[len(out)-len(b):]
	for i := range b {
		if b[i] != tail[i] {
			return false
		}
	}
	return true
}

func (g *Graph) base(i int) int {
	for g.Values[i].alias >= 0 {
		i = g.Values[i].alias
	}
	return i
}

// consumers conta i nodi che leggono ogni valore (viste comprese) e segna le
// uscite.
func (g *Graph) consumers() []int {
	use := make([]int, len(g.Values))
	for i := range g.Nodes {
		n := &g.Nodes[i]
		for _, v := range n.In {
			use[g.base(v)]++
		}
		if n.Res >= 0 {
			use[g.base(n.Res)]++
		}
	}
	for _, o := range g.Outputs {
		use[g.base(o)] += 1 << 20
	}
	return use
}

// fusePad porta nelle convoluzioni il padding spaziale a zero del PAD che le
// precede (se ne è l'unico consumatore): un tensore copiato in meno.
func (g *Graph) fusePad() {
	use := g.consumers()
	producer := map[int]int{}
	for i := range g.Nodes {
		producer[g.Nodes[i].Out] = i
	}
	drop := map[int]bool{}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Kind != KConv && n.Kind != KDepthwise {
			continue
		}
		x := n.In[0]
		pi, ok := producer[x]
		if !ok || g.Values[x].alias >= 0 || use[x] != 1 {
			continue
		}
		p := &g.Nodes[pi]
		if p.Kind != KPad || p.PadValue != 0 || p.Pad[0] != 0 || p.Pad[3] != 0 {
			continue
		}
		src := &g.Values[p.In[0]]
		pv := &g.Values[x]
		if src.Shape[3] != pv.Shape[3] {
			continue
		}
		n.PT += p.Pad[1]
		n.PL += p.Pad[2]
		n.PB += pv.Shape[1] - src.Shape[1] - p.Pad[1]
		n.PR += pv.Shape[2] - src.Shape[2] - p.Pad[2]
		n.In[0] = p.In[0]
		drop[pi] = true
	}
	g.compact(drop)
}

// fuseResidual porta nella convoluzione la somma residua (ADD) che ne segue,
// con la sua attivazione, se la convoluzione ne è l'unico consumatore e
// l'altro addendo è già pronto quando la convoluzione viene eseguita.
func (g *Graph) fuseResidual() {
	use := g.consumers()
	producer := map[int]int{}
	for i := range g.Nodes {
		producer[g.Nodes[i].Out] = i
	}
	drop := map[int]bool{}
	for i := range g.Nodes {
		a := &g.Nodes[i]
		if a.Kind != KBinary || a.BinOp != BinAdd || g.Values[a.In[0]].Elems() != g.Values[a.In[1]].Elems() {
			continue
		}
		for side := 0; side < 2; side++ {
			c, r := a.In[side], a.In[1-side]
			ci, ok := producer[c]
			if !ok || g.Values[c].alias >= 0 || use[c] != 1 || drop[ci] {
				continue
			}
			cn := &g.Nodes[ci]
			if (cn.Kind != KConv && cn.Kind != KDepthwise) || cn.Res >= 0 {
				continue
			}
			rb := g.base(r)
			if ri, ok := producer[rb]; ok && ri >= ci {
				continue // l'altro addendo arriva dopo la convoluzione
			}
			if g.Values[rb].Const {
				continue
			}
			cn.Res, cn.Act2, cn.Out = r, a.Act, a.Out
			drop[i] = true
			break
		}
	}
	g.compact(drop)
}

func (g *Graph) compact(drop map[int]bool) {
	if len(drop) == 0 {
		return
	}
	kept := g.Nodes[:0]
	for i := range g.Nodes {
		if !drop[i] {
			kept = append(kept, g.Nodes[i])
		}
	}
	g.Nodes = kept
}

// prepareWeights porta i pesi delle convoluzioni nel layout HWIO
// ([KH][KW][Cin][Cout], per le 1×1 [Cin][Cout]): i canali d'uscita sono
// contigui, come serve ai kernel. Le convoluzioni 1×1 a passo 1 e quelle che
// coprono tutto l'ingresso (le teste finali, di fatto fully-connected)
// diventano prodotti di matrici. Conta le moltiplicazioni-accumulo.
func (g *Graph) prepareWeights() {
	wuse := map[int]int{}
	for i := range g.Nodes {
		wuse[g.Nodes[i].W]++
	}
	g.MACs = 0
	for i := range g.Nodes {
		n := &g.Nodes[i]
		switch n.Kind {
		case KConv:
			w := &g.Values[n.W]
			cout, cin := w.Shape[0], w.Shape[3]
			is, os := g.Values[n.In[0]].Shape, g.Values[n.Out].Shape
			n.MACs = int64(elems(os)) * int64(n.KH*n.KW*cin)
			pads := n.PT + n.PL + n.PB + n.PR
			if (n.KH == 1 && n.KW == 1 && n.SH == 1 && n.SW == 1 && pads == 0) ||
				(n.KH == is[1] && n.KW == is[2] && os[1] == 1 && os[2] == 1 && pads == 0 && n.DH == 1 && n.DW == 1) {
				n.Kind = KPointwise
			}
			if wuse[n.W] > 1 {
				// pesi condivisi: copia propria nel nuovo layout
				c := g.Values[n.W]
				g.Values = append(g.Values, c)
				n.W = len(g.Values) - 1
				w = &g.Values[n.W]
			}
			k := len(w.Data) / cout // KH·KW·Cin
			t := make([]float32, len(w.Data))
			for o := 0; o < cout; o++ {
				for j := 0; j < k; j++ {
					t[j*cout+o] = w.Data[o*k+j]
				}
			}
			w.Data = t
		case KDepthwise:
			n.MACs = int64(g.Values[n.Out].Elems()) * int64(n.KH*n.KW)
		}
		g.MACs += n.MACs
	}
}

// GemmShape sono le dimensioni del prodotto di matrici di un nodo
// KPointwise: M righe (pixel), K ingressi, N uscite.
func (g *Graph) GemmShape(n *Node) (M, K, N int) {
	os := g.Values[n.Out].Shape
	N = os[len(os)-1]
	M = g.Values[n.Out].Elems() / N
	K = g.Values[n.In[0]].Elems() / M
	return
}

// allocate assegna gli offset: pesi in testa, poi i tensori intermedi con un
// first-fit sui tempi di vita (i più grandi prima). Offset allineati a 16 B.
func (g *Graph) allocate() {
	const align = 4
	up := func(n int) int { return (n + align - 1) &^ (align - 1) }
	off := 0
	used := map[int]bool{}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		used[n.W], used[n.B], used[n.Res] = true, true, true
		for _, v := range n.In {
			used[v] = true
		}
	}
	for i := range g.Values {
		v := &g.Values[i]
		if v.Const && used[i] {
			v.Off = off
			off = up(off + len(v.Data))
		}
	}
	g.Weights = off

	// tempi di vita dei valori base
	for i := range g.Values {
		g.Values[i].first, g.Values[i].last = -1, -1
	}
	in := g.base(g.Input)
	g.Values[in].first, g.Values[in].last = -1, 0
	touch := func(v, at int) {
		b := g.base(v)
		if g.Values[b].last < at {
			g.Values[b].last = at
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		ob := g.base(n.Out)
		if g.Values[ob].first < 0 && ob != in {
			g.Values[ob].first = i
		}
		touch(n.Out, i)
		for _, v := range n.In {
			touch(v, i)
		}
		if n.Res >= 0 {
			touch(n.Res, i)
		}
	}
	end := len(g.Nodes)
	for _, o := range g.Outputs {
		touch(o, end)
		g.Values[g.base(o)].output = true
	}
	// l'ingresso resta leggibile dopo l'esecuzione (Runner.Input)
	touch(g.Input, end)
	type item struct{ v, size, first, last int }
	var items []item
	for i := range g.Values {
		v := &g.Values[i]
		if v.Const || v.alias >= 0 || (v.first < 0 && i != in) {
			continue
		}
		items = append(items, item{i, up(v.Elems()), v.first, v.last})
	}
	sort.SliceStable(items, func(a, b int) bool { return items[a].size > items[b].size })
	type placed struct{ off, size, first, last int }
	var done []placed
	top := g.Weights
	for _, it := range items {
		// intervalli occupati da valori vivi nello stesso tempo
		var busy []placed
		for _, p := range done {
			if p.first <= it.last && it.first <= p.last {
				busy = append(busy, p)
			}
		}
		sort.Slice(busy, func(a, b int) bool { return busy[a].off < busy[b].off })
		o := g.Weights
		for _, p := range busy {
			if o+it.size <= p.off {
				break
			}
			o = max(o, p.off+p.size)
		}
		g.Values[it.v].Off = o
		done = append(done, placed{o, it.size, it.first, it.last})
		top = max(top, o+it.size)
	}
	for i := range g.Values {
		if v := &g.Values[i]; v.alias >= 0 {
			v.Off = g.Values[g.base(i)].Off
		}
	}
	g.Arena = top
}

// Off è l'offset di un valore (anche viste) nell'arena.
func (g *Graph) Off(v int) int { return g.Values[g.base(v)].Off }

// InputShape è la forma dell'ingresso [1 H W C].
func (g *Graph) InputShape() []int { return g.Values[g.Input].Shape }

// OutputShape è la forma dell'uscita i.
func (g *Graph) OutputShape(i int) []int { return g.Values[g.Outputs[i]].Shape }

// WeightData scrive i pesi nell'arena (il chiamante la alloca di Arena float).
func (g *Graph) WeightData(arena []float32) {
	if g.released {
		panic("tflite: pesi del piano già rilasciati")
	}
	for i := range g.Values {
		v := &g.Values[i]
		if v.Const && v.Off >= 0 {
			copy(arena[v.Off:], v.Data)
		}
	}
}

// ReleaseWeights libera la copia dei pesi del piano: i backend li copiano
// in Load, dopo quindi bastano forme e offset. Un Load successivo dello
// stesso piano non è più possibile.
func (g *Graph) ReleaseWeights() {
	for i := range g.Values {
		if g.Values[i].Const {
			g.Values[i].Data = nil
		}
	}
	g.released = true
}

// Stats riassume il piano.
func (g *Graph) Stats() string {
	return fmt.Sprintf("%d operatori TFLite → %d nodi · %.1f MMAC · pesi %.1f MB · attivazioni %.1f MB",
		g.Ops, len(g.Nodes), float64(g.MACs)/1e6, float64(g.Weights*4)/(1<<20), float64((g.Arena-g.Weights)*4)/(1<<20))
}

func f32bits(f float32) uint32 { return math.Float32bits(f) }

// Elems è il numero di elementi di una forma.
func Elems(shape []int) int { return elems(shape) }
