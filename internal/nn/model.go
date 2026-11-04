package nn

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
)

// Model è un grafo ONNX compilato in una sequenza di passi con buffer
// pre-allocati: Run non alloca memoria per i tensori. Non è sicuro per l'uso
// concorrente: va usato da una sola goroutine.
type Model struct {
	steps   []step
	input   *value
	outputs map[string]*value
	workers int
	macs    int64
	bufMB   float64

	tasks chan task // pool di goroutine permanenti per par
	wg    sync.WaitGroup
	pool  int
}

type task struct {
	fn     func(lo, hi int)
	lo, hi int
}

type value struct {
	name    string
	shape   []int
	data    []float32
	i64     []int64
	konst   bool
	lastUse int
	keep    bool // ingresso o uscita del grafo: il buffer non va riusato
}

type step struct {
	op  string
	in  []*value
	out *value

	// Conv / MaxPool
	kh, kw, sh, sw       int
	pt, pl, pb, pr       int
	group                int
	perm                 []int // Transpose
	pads                 []int // Pad: inizio per asse, poi fine per asse
	slopePerChannel      bool  // PRelu
	pointwise, depthwise bool

	// prelu, se non nil, è una PRelu fusa nell'uscita di Conv o Add
	// (pendenza per canale, o una sola se len == 1).
	prelu []float32
	col   []float32 // buffer im2col delle convoluzioni generiche

	// fns: funzioni passate a par, create alla prima esecuzione del passo
	// (catturano solo buffer e forme, che non cambiano): Run non alloca.
	fns [2]func(lo, hi int)
}

func size(shape []int) int {
	n := 1
	for _, d := range shape {
		n *= d
	}
	return n
}

// Parse compila un modello ONNX già in memoria.
func Parse(data []byte, inputShape []int) (*Model, error) {
	g, err := parseModel(data)
	if err != nil {
		return nil, err
	}
	var graphInputs []string
	for _, n := range g.inputs {
		if _, ok := g.inits[n]; !ok {
			graphInputs = append(graphInputs, n)
		}
	}
	if len(graphInputs) != 1 {
		return nil, fmt.Errorf("onnx: %d graph inputs, want 1", len(graphInputs))
	}
	if len(inputShape) == 0 || inputShape[0] != 1 {
		return nil, errors.New("onnx: input shape must start with batch 1")
	}
	m := &Model{outputs: map[string]*value{}, workers: min(runtime.NumCPU(), 4)}
	vals := map[string]*value{}
	for name, t := range g.inits {
		vals[name] = &value{name: name, shape: t.dims, data: t.f32, i64: t.i64, konst: true}
	}
	in := &value{name: graphInputs[0], shape: append([]int(nil), inputShape...), keep: true}
	in.data = make([]float32, size(in.shape))
	vals[in.name] = in
	m.input = in

	for i, n := range g.nodes {
		if n.domain != "" && n.domain != "ai.onnx" {
			return nil, fmt.Errorf("onnx: node %d: unsupported domain %q", i, n.domain)
		}
		st := step{op: n.op}
		for _, name := range n.in {
			if name == "" {
				st.in = append(st.in, nil)
				continue
			}
			v, ok := vals[name]
			if !ok {
				return nil, fmt.Errorf("onnx: node %d (%s): unknown input %q", i, n.op, name)
			}
			v.lastUse = i
			st.in = append(st.in, v)
		}
		if len(n.out) != 1 {
			return nil, fmt.Errorf("onnx: node %d (%s): %d outputs, want 1", i, n.op, len(n.out))
		}
		shape, err := st.compile(n)
		if err != nil {
			return nil, fmt.Errorf("onnx: node %d (%s): %w", i, n.op, err)
		}
		st.out = &value{name: n.out[0], shape: shape, lastUse: i}
		vals[st.out.name] = st.out
		m.steps = append(m.steps, st)
		m.macs += st.cost()
	}
	for _, name := range g.outputs {
		v, ok := vals[name]
		if !ok || v.konst {
			return nil, fmt.Errorf("onnx: graph output %q not produced", name)
		}
		v.keep = true
		m.outputs[name] = v
	}
	m.fusePRelu()
	m.allocate()
	return m, nil
}

// fusePRelu fonde ogni PRelu nel Conv o Add che la precede quando ne è
// l'unico consumatore: un passaggio sulla memoria in meno per strato.
func (m *Model) fusePRelu() {
	uses := map[*value]int{}
	for i := range m.steps {
		for _, v := range m.steps[i].in {
			uses[v]++
		}
	}
	producer := map[*value]*step{}
	kept := m.steps[:0]
	for i := range m.steps {
		st := m.steps[i]
		if st.op == "PRelu" {
			x := st.in[0]
			if p, ok := producer[x]; ok && uses[x] == 1 && !x.keep && p.prelu == nil && st.in[1].konst &&
				(p.op == "Conv" || p.op == "Add") {
				p.prelu = st.in[1].data
				p.out = st.out
				producer[st.out] = p
				continue
			}
		}
		kept = append(kept, st)
		producer[kept[len(kept)-1].out] = &kept[len(kept)-1]
	}
	m.steps = kept
	// Il riuso dei buffer dipende dall'ultimo passo che legge ogni valore.
	for i := range m.steps {
		st := &m.steps[i]
		st.out.lastUse = i
		for _, v := range st.in {
			if v != nil {
				v.lastUse = i
			}
		}
	}
}

// allocate assegna i buffer riusando quelli dei valori non più necessari.
func (m *Model) allocate() {
	free := map[int][][]float32{}
	total := 0
	for i := range m.steps {
		st := &m.steps[i]
		n := size(st.out.shape)
		if st.out.keep {
			st.out.data = make([]float32, n)
			total += n
		} else if l := free[n]; len(l) > 0 {
			st.out.data, free[n] = l[len(l)-1], l[:len(l)-1]
		} else {
			st.out.data = make([]float32, n)
			total += n
		}
		for _, v := range st.in {
			if v != nil && !v.konst && !v.keep && v.lastUse == i {
				free[len(v.data)] = append(free[len(v.data)], v.data)
			}
		}
		if st.out.lastUse == i && !st.out.keep { // valore mai letto
			free[n] = append(free[n], st.out.data)
		}
	}
	m.bufMB = float64(total*4) / (1 << 20)
}

// SetWorkers imposta le goroutine usate dalle convoluzioni (minimo 1).
func (m *Model) SetWorkers(n int) { m.workers = max(n, 1) }

// Input è il buffer dell'ingresso, da riempire prima di Run.
func (m *Model) Input() []float32 { return m.input.data }

// InputShape è la forma dell'ingresso.
func (m *Model) InputShape() []int { return m.input.shape }

// Output restituisce il buffer di un'uscita del grafo (valido fino al Run
// successivo).
func (m *Model) Output(name string) ([]float32, error) {
	v, ok := m.outputs[name]
	if !ok {
		return nil, fmt.Errorf("nn: no output %q", name)
	}
	return v.data, nil
}

// MACs è il numero di moltiplicazioni-accumulo per inferenza; BufferMB la
// memoria dei tensori intermedi.
func (m *Model) MACs() int64       { return m.macs }
func (m *Model) BufferMB() float64 { return m.bufMB }

// Run esegue il grafo.
func (m *Model) Run() {
	for i := range m.steps {
		m.exec(&m.steps[i])
	}
}

// par divide [0,n) tra le goroutine di lavoro (un pool permanente: nessuna
// goroutine né allocazione per chiamata).
func (m *Model) par(n int, fn func(lo, hi int)) {
	w := min(m.workers, n)
	if w <= 1 {
		fn(0, n)
		return
	}
	if m.pool < m.workers {
		if m.tasks == nil {
			m.tasks = make(chan task, 64)
		}
		for ; m.pool < m.workers; m.pool++ {
			go func() {
				for t := range m.tasks {
					t.fn(t.lo, t.hi)
					m.wg.Done()
				}
			}()
		}
	}
	chunk := (n + w - 1) / w
	for lo := 0; lo < n; lo += chunk {
		m.wg.Add(1)
		m.tasks <- task{fn: fn, lo: lo, hi: min(lo+chunk, n)}
	}
	m.wg.Wait()
}

// Close ferma le goroutine del pool.
func (m *Model) Close() {
	if m.tasks != nil {
		close(m.tasks)
		m.tasks = nil
		m.pool = 0
	}
}
