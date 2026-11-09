//go:build linux

package tflite

import (
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"github.com/ablescia/tobii4c-linux/internal/vk"
)

//go:generate sh shaders/build.sh

//go:embed spv/*.spv
var spirv embed.FS

// Il buffer dell'immagine condiviso tra CPU e GPU: in testa i parametri
// dell'ingresso di ogni modello (slot da 16 parole), poi i piani Y, Cb, Cr.
const (
	slotWords = 16
	maxSlots  = 32
	imgBase   = slotWords * maxSlots * 4 // byte
)

// GPU è il backend Vulkan: ogni modello ha un'arena nella memoria della
// GPU e un command buffer registrato una volta (warp dell'immagine, nodi,
// copia delle uscite), rieseguito a ogni inferenza.
type GPU struct {
	dev     *vk.Device
	pipes   map[string]*vk.Pipeline
	img     *vk.Buffer
	runners []*gpuRunner
	slots   int

	// layout dell'immagine corrente (byte nel buffer)
	im                       Image
	yOff, cbOff, crOff, have int
	// Profile registra un timestamp dopo ogni nodo (vedi NodeTimes).
	Profile bool
}

// NewGPU apre la GPU (vedi vk.Open) e prepara le pipeline.
func NewGPU() (*GPU, error) {
	dev, err := vk.Open()
	if err != nil {
		return nil, err
	}
	b := &GPU{dev: dev, pipes: map[string]*vk.Pipeline{}}
	ents, err := spirv.ReadDir("spv")
	if err != nil {
		b.Close()
		return nil, err
	}
	for _, e := range ents {
		code, err := spirv.ReadFile("spv/" + e.Name())
		if err != nil {
			b.Close()
			return nil, err
		}
		p, err := dev.NewPipeline(code)
		if err != nil {
			b.Close()
			return nil, fmt.Errorf("shader %s: %w", e.Name(), err)
		}
		b.pipes[strings.TrimSuffix(e.Name(), ".spv")] = p
	}
	// posto per un frame 1920×1080 con la crominanza piena: di norma il
	// buffer non deve mai crescere (crescendo i programmi vanno registrati
	// di nuovo)
	if b.img, err = dev.NewBuffer(imgBase+3*1920*1080, true); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *GPU) Name() string { return "GPU" }

func (b *GPU) Device() string { return b.dev.Info.Name }

// Info descrive la GPU.
func (b *GPU) Info() vk.Info { return b.dev.Info }

// Close libera tutto, modelli compresi.
func (b *GPU) Close() {
	for _, r := range b.runners {
		r.destroy()
	}
	b.runners = nil
	for _, p := range b.pipes {
		p.Destroy()
	}
	b.img.Destroy()
	b.dev.Close()
}

// SetImage copia i piani dell'immagine nel buffer condiviso; se non c'è
// posto il buffer cresce e i programmi dei modelli vengono registrati di
// nuovo.
func (b *GPU) SetImage(im *Image) error {
	if im == nil || im.W <= 0 || im.H <= 0 || len(im.Y) < im.W*im.H {
		return errors.New("tflite: immagine non valida")
	}
	gray := im.Gray()
	if !gray && (len(im.Cb) < im.CW*im.CH || len(im.Cr) < im.CW*im.CH) {
		return errors.New("tflite: crominanza non valida")
	}
	up := func(n int) int { return (n + 3) &^ 3 }
	ny := im.W * im.H
	nc := 0
	if !gray {
		nc = im.CW * im.CH
	}
	need := imgBase + up(ny) + 2*up(nc)
	if need > b.img.Size {
		nb, err := b.dev.NewBuffer(need+need/4, true)
		if err != nil {
			return err
		}
		old := b.img
		b.img = nb
		for _, r := range b.runners {
			if err := r.record(); err != nil {
				return err
			}
		}
		old.Destroy()
	}
	buf := b.img.Bytes()
	b.yOff = imgBase
	copy(buf[b.yOff:], im.Y[:ny])
	b.im = Image{W: im.W, H: im.H}
	if !gray {
		b.cbOff = b.yOff + up(ny)
		b.crOff = b.cbOff + up(nc)
		copy(buf[b.cbOff:], im.Cb[:nc])
		copy(buf[b.crOff:], im.Cr[:nc])
		b.im.CW, b.im.CH = im.CW, im.CH
	}
	b.have = 1
	return nil
}

type gpuRunner struct {
	b     *GPU
	g     *Graph
	arena *vk.Buffer // memoria della GPU
	host  *vk.Buffer // ingresso (RunTensor) e uscite
	set   *vk.Set
	prog  *vk.Program // ingresso dall'immagine
	progT *vk.Program // ingresso dal tensore, creato al primo RunTensor
	slot  int
	inOff int   // float nell'host buffer
	outs  []int // float nell'host buffer
	tm    Timing
	ts    []float64
}

// Load carica un modello: pesi nella memoria della GPU e registrazione dei
// comandi.
func (b *GPU) Load(g *Graph) (Runner, error) {
	if b.slots >= maxSlots {
		return nil, errors.New("tflite: troppi modelli sulla GPU")
	}
	r := &gpuRunner{b: b, g: g, slot: b.slots}
	var err error
	if r.arena, err = b.dev.NewBuffer(4*g.Arena, false); err != nil {
		return nil, err
	}
	hostF := g.Values[g.Input].Elems()
	r.inOff = 0
	for _, o := range g.Outputs {
		hostF = (hostF + 3) &^ 3
		r.outs = append(r.outs, hostF)
		hostF += g.Values[o].Elems()
	}
	if r.host, err = b.dev.NewBuffer(4*hostF, true); err != nil {
		r.destroy()
		return nil, err
	}
	w := make([]float32, g.Weights)
	g.WeightData(w)
	if len(w) > 0 {
		if err := b.dev.Upload(r.arena, 0, unsafe.Slice((*byte)(unsafe.Pointer(&w[0])), 4*len(w))); err != nil {
			r.destroy()
			return nil, err
		}
	}
	if err := r.record(); err != nil {
		r.destroy()
		return nil, err
	}
	b.slots++
	b.runners = append(b.runners, r)
	return r, nil
}

func (r *gpuRunner) destroy() {
	r.prog.Destroy()
	r.progT.Destroy()
	r.prog, r.progT = nil, nil
	r.arena.Destroy()
	r.host.Destroy()
}

// record (ri)registra i programmi; serve di nuovo quando cambia il buffer
// dell'immagine.
func (r *gpuRunner) record() error {
	b := r.b
	set, err := b.dev.NewSet(r.arena, b.img)
	if err != nil {
		return err
	}
	r.set = set
	if r.prog != nil {
		r.prog.Destroy()
		r.prog = nil
	}
	if r.progT != nil {
		r.progT.Destroy()
		r.progT = nil
	}
	is := r.g.InputShape()
	r.prog, err = r.program(func(rec *vk.Recorder) {
		if is[3] == 3 {
			rec.Dispatch(b.pipes["warp"], []uint32{uint32(r.slot * slotWords), uint32(r.g.Off(r.g.Input)), uint32(is[2]), uint32(is[1])},
				(is[2]+15)/16, (is[1]+15)/16, 1)
		}
	})
	return err
}

func (r *gpuRunner) nts() int {
	if r.b.Profile {
		return len(r.g.Nodes) + 2
	}
	return 2
}

// program registra: ingresso (in), nodi, copia delle uscite.
func (r *gpuRunner) program(in func(rec *vk.Recorder)) (*vk.Program, error) {
	var ferr error
	p, err := r.b.dev.Record(r.nts(), func(rec *vk.Recorder) {
		rec.Bind(r.set)
		rec.Timestamp(0)
		in(rec)
		for i := range r.g.Nodes {
			if err := r.node(rec, &r.g.Nodes[i]); err != nil && ferr == nil {
				ferr = err
			}
			if r.b.Profile {
				rec.Timestamp(i + 1)
			}
		}
		for k, o := range r.g.Outputs {
			rec.Copy(r.arena, 4*r.g.Off(o), r.host, 4*r.outs[k], 4*r.g.Values[o].Elems())
		}
		rec.Timestamp(r.nts() - 1)
	})
	if ferr != nil {
		p.Destroy()
		return nil, ferr
	}
	return p, err
}

func u(v int) uint32 { return uint32(v) }

func opt(v int, g *Graph) uint32 {
	if v < 0 {
		return 0xffffffff
	}
	return uint32(g.Off(v))
}

// grid copre n thread con workgroup da local, su due assi se servono più di
// 65535 workgroup.
func grid(n, local int) (int, int) {
	groups := (n + local - 1) / local
	gx := min(groups, 65535)
	return gx, (groups + gx - 1) / gx
}

func (r *gpuRunner) node(rec *vk.Recorder, n *Node) error {
	g, P := r.g, r.b.pipes
	os := g.Values[n.Out].Shape
	y := u(g.Off(n.Out))
	shape := func(v int) []int { return g.Values[v].Shape }
	switch n.Kind {
	case KPointwise:
		M, K, N := g.GemmShape(n)
		push := []uint32{u(M), u(K), u(N), u(g.Off(n.In[0])), u(g.Off(n.W)), opt(n.B, g), y, opt(n.Res, g), u(n.Act), u(n.Act2)}
		if tiles := ((M + 63) / 64) * ((N + 63) / 64); tiles >= 128 {
			rec.Dispatch(P["pw"], push, (N+63)/64, (M+63)/64, 1)
		} else {
			// poche tile: un workgroup per pixel e 64 uscite
			rec.Dispatch(P["pwrow"], push, (N+63)/64, min(M, 65535), (M+65534)/65535)
		}
	case KConv:
		is := shape(n.In[0])
		gx, gy := grid(os[1]*os[2]*os[3], 64)
		rec.Dispatch(P["conv"], []uint32{u(is[1]), u(is[2]), u(is[3]), u(os[1]), u(os[2]), u(os[3]), u(n.KH), u(n.KW),
			u(n.SH), u(n.SW), u(n.PT), u(n.PL), u(n.DH), u(n.DW), u(g.Off(n.In[0])), u(g.Off(n.W)), opt(n.B, g), y,
			opt(n.Res, g), u(n.Act), u(n.Act2)}, gx, gy, 1)
	case KDepthwise:
		is := shape(n.In[0])
		gx, gy := grid(os[1]*os[2]*os[3], 64)
		rec.Dispatch(P["dw"], []uint32{u(is[1]), u(is[2]), u(is[3]), u(os[1]), u(os[2]), u(n.KH), u(n.KW), u(n.SH), u(n.SW),
			u(n.PT), u(n.PL), u(n.DH), u(n.DW), u(g.Off(n.In[0])), u(g.Off(n.W)), opt(n.B, g), y, opt(n.Res, g),
			u(n.Act), u(n.Act2)}, gx, gy, 1)
	case KPool:
		is := shape(n.In[0])
		gx, gy := grid(os[1]*os[2]*os[3], 64)
		isMax := 0
		if n.Max {
			isMax = 1
		}
		rec.Dispatch(P["pool"], []uint32{u(is[1]), u(is[2]), u(is[3]), u(os[1]), u(os[2]), u(n.KH), u(n.KW), u(n.SH), u(n.SW),
			u(n.PT), u(n.PL), u(g.Off(n.In[0])), y, u(isMax), u(n.Act)}, gx, gy, 1)
	case KBinary:
		total := g.Values[n.Out].Elems()
		gx, gy := grid(total, 256)
		rec.Dispatch(P["ew"], []uint32{u(total), u(g.Off(n.In[0])), u(g.Off(n.In[1])), y, u(n.NB), u(n.BinOp), u(n.Act)}, gx, gy, 1)
	case KUnary:
		total := g.Values[n.Out].Elems()
		gx, gy := grid(total, 256)
		rec.Dispatch(P["ew"], []uint32{u(total), u(g.Off(n.In[0])), u(g.Off(n.In[0])), y, 1, 10, u(n.Act)}, gx, gy, 1)
	case KPad:
		is := shape(n.In[0])
		gx, gy := grid(os[1]*os[2]*os[3], 256)
		rec.Dispatch(P["pad"], []uint32{u(is[1]), u(is[2]), u(is[3]), u(os[1]), u(os[2]), u(os[3]), u(n.Pad[1]), u(n.Pad[2]),
			u(n.Pad[3]), u(g.Off(n.In[0])), y, f32bits(n.PadValue)}, gx, gy, 1)
	case KResize:
		is := shape(n.In[0])
		gx, gy := grid(os[1]*os[2]*os[3], 256)
		half := 0
		if n.HalfPixel {
			half = 1
		}
		rec.Dispatch(P["resize"], []uint32{u(is[1]), u(is[2]), u(is[3]), u(os[1]), u(os[2]), u(g.Off(n.In[0])), y,
			f32bits(resizeScale(is[1], os[1], n.AlignCorners)), f32bits(resizeScale(is[2], os[2], n.AlignCorners)), u(half)}, gx, gy, 1)
	case KDepthToSpace:
		is := shape(n.In[0])
		gx, gy := grid(os[1]*os[2]*os[3], 256)
		rec.Dispatch(P["d2s"], []uint32{u(is[1]), u(is[2]), u(is[3]), u(n.Block), u(g.Off(n.In[0])), y}, gx, gy, 1)
	case KConcat:
		outer, inner := 1, 1
		for a := 0; a < n.Axis; a++ {
			outer *= os[a]
		}
		for a := n.Axis + 1; a < len(os); a++ {
			inner *= os[a]
		}
		outA := os[n.Axis] * inner
		off := 0
		for _, v := range n.In {
			inA := shape(v)[n.Axis] * inner
			gx, gy := grid(outer*inA, 256)
			rec.Dispatch(P["copy"], []uint32{u(outer), u(inA), u(outA), u(off), u(g.Off(v)), y}, gx, gy, 1)
			off += inA
		}
	default:
		return fmt.Errorf("tflite: nodo %s senza kernel GPU", n.Kind)
	}
	return nil
}

func (r *gpuRunner) Graph() *Graph  { return r.g }
func (r *gpuRunner) Timing() Timing { return r.tm }
func (r *gpuRunner) Close()         {}

func (r *gpuRunner) RunImage(w *Warp) error {
	b := r.b
	if b.have == 0 {
		return errors.New("tflite: nessuna immagine")
	}
	t0 := time.Now()
	p := b.img.Bytes()[4*r.slot*slotWords:]
	put := func(i int, v uint32) {
		p[4*i], p[4*i+1], p[4*i+2], p[4*i+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	flags := uint32(0)
	if b.im.CW > 0 {
		flags |= 1
	}
	if w.Replicate {
		flags |= 2
	}
	for i, v := range []uint32{u(b.im.W), u(b.im.H), u(b.yOff), u(b.im.CW), u(b.im.CH), u(b.cbOff), u(b.crOff), flags} {
		put(i, v)
	}
	for i, v := range w.M {
		put(8+i, f32bits(v))
	}
	put(14, f32bits(w.Lo))
	put(15, f32bits(w.Hi))
	if err := r.prog.Run(); err != nil {
		return err
	}
	r.timing(r.prog, t0)
	return nil
}

func (r *gpuRunner) RunTensor(in []float32) error {
	n := r.g.Values[r.g.Input].Elems()
	if len(in) != n {
		return fmt.Errorf("tflite: ingresso di %d float, attesi %d", len(in), n)
	}
	if r.progT == nil {
		var err error
		r.progT, err = r.program(func(rec *vk.Recorder) {
			rec.Copy(r.host, 4*r.inOff, r.arena, 4*r.g.Off(r.g.Input), 4*n)
		})
		if err != nil {
			return err
		}
	}
	t0 := time.Now()
	copy(r.host.Float32()[r.inOff:], in)
	if err := r.progT.Run(); err != nil {
		return err
	}
	r.timing(r.progT, t0)
	return nil
}

func (r *gpuRunner) timing(p *vk.Program, t0 time.Time) {
	r.tm = Timing{Wall: time.Since(t0)}
	if p.HasTimestamps() {
		if len(r.ts) < r.nts() {
			r.ts = make([]float64, r.nts())
		}
		if p.Timestamps(r.ts) == nil {
			r.tm.GPU = time.Duration(r.ts[r.nts()-1])
		}
	}
}

// NodeTimes sono i tempi GPU di ogni nodo dell'ultima esecuzione (solo con
// Profile attivo al caricamento).
func (r *gpuRunner) NodeTimes() []time.Duration {
	if !r.b.Profile || len(r.ts) < len(r.g.Nodes)+2 {
		return nil
	}
	out := make([]time.Duration, len(r.g.Nodes))
	for i := range out {
		out[i] = time.Duration(r.ts[i+1] - r.ts[i])
	}
	return out
}

func (r *gpuRunner) Output(i int) []float32 {
	v := r.g.Outputs[i]
	return r.host.Float32()[r.outs[i] : r.outs[i]+r.g.Values[v].Elems()]
}

func (r *gpuRunner) Input() ([]float32, error) {
	n := r.g.Values[r.g.Input].Elems()
	tmp, err := r.b.dev.NewBuffer(4*n, true)
	if err != nil {
		return nil, err
	}
	defer tmp.Destroy()
	p, err := r.b.dev.Record(0, func(rec *vk.Recorder) { rec.Copy(r.arena, 4*r.g.Off(r.g.Input), tmp, 0, 4*n) })
	if err != nil {
		return nil, err
	}
	defer p.Destroy()
	if err := p.Run(); err != nil {
		return nil, err
	}
	return append([]float32(nil), tmp.Float32()[:n]...), nil
}
