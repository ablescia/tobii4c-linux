//go:build linux

package webcam

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Decodifica MJPEG → piani YCbCr, al posto di image/jpeg per i frame delle
// webcam. Con Decode serve solo il piano Y: i blocchi di crominanza vanno
// letti (la codifica di Huffman è sequenziale) ma non vengono dequantizzati
// né trasformati; DecodeColor scrive anche Cb e Cr alla loro risoluzione
// (sottocampionati come nel JPEG). In regime nessun frame alloca memoria
// (image/jpeg alloca l'immagine intera a ogni frame).
//
// Le webcam UVC mettono quasi sempre un marker di restart ogni poche MCU (la
// Logitech C925e ogni 10: 720 tratti per frame a 1280×720). I tratti tra due
// restart sono indipendenti (i predittori del DC ripartono da zero), quindi
// vengono divisi tra più goroutine.
//
// Gestisce i JPEG baseline e sequenziali estesi a 8 bit con codifica di
// Huffman e una sola scansione, cioè quelli delle webcam; per gli altri
// restituisce errUnsupportedJPEG e il chiamante ripiega su image/jpeg.

var errUnsupportedJPEG = errors.New("JPEG non gestito dal decoder veloce")

// lutBits sono i bit della tabella di consultazione di Huffman: i codici più
// corti (quasi tutti) si decodificano con un solo accesso.
const lutBits = 10

// unzig porta l'indice zig-zag nell'ordine naturale del blocco 8×8.
var unzig = [64]uint8{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}

type huffTable struct {
	ok  bool
	lut [1 << lutBits]uint16 // (lunghezza << 8) | simbolo; 0: codice più lungo di lutBits
	// fast, per i coefficienti AC: codice e bit del valore in un solo
	// accesso quando ci stanno entrambi in lutBits. valore << 16 |
	// zeri prima del coefficiente << 8 | bit totali; 0: via normale.
	fast    [1 << lutBits]int32
	maxcode [17]int32 // per lunghezza: il codice più alto, -1 se non ce ne sono
	delta   [17]int32 // indice in vals del codice c di quella lunghezza: c + delta
	vals    [256]uint8
}

// build costruisce i codici canonici dalla tabella DHT: counts[i] codici
// di lunghezza i+1, con i simboli in vals.
func (t *huffTable) build(counts []byte, vals []byte) error {
	t.ok = false
	total := 0
	for _, c := range counts {
		total += int(c)
	}
	if total == 0 || total > 256 || len(vals) < total {
		return errors.New("tabella di Huffman non valida")
	}
	copy(t.vals[:], vals[:total])
	clear(t.lut[:])
	code, k := int32(0), 0
	for l := 1; l <= 16; l++ {
		n := int(counts[l-1])
		t.maxcode[l], t.delta[l] = -1, 0
		if n > 0 {
			t.delta[l] = int32(k) - code
			if code+int32(n) > 1<<l {
				return errors.New("tabella di Huffman non valida")
			}
			for i := 0; i < n; i++ {
				if l <= lutBits {
					shift := lutBits - l
					e := uint16(l)<<8 | uint16(t.vals[k])
					base := int(code) << shift
					for j := 0; j < 1<<shift; j++ {
						t.lut[base+j] = e
					}
				}
				code++
				k++
			}
			t.maxcode[l] = code - 1
		}
		code <<= 1
	}
	// tabella combinata: per ogni prefisso con un codice corto, se ci stanno
	// anche i bit del valore il coefficiente è già deciso
	for i, e := range t.lut {
		t.fast[i] = 0
		if e == 0 {
			continue
		}
		l, rs := uint(e>>8), uint8(e)
		r, sz := uint(rs>>4), uint(rs&15)
		if sz == 0 || l+sz > lutBits {
			continue
		}
		v := int32(uint(i)>>(lutBits-l-sz)) & (1<<sz - 1)
		if v < 1<<(sz-1) {
			v += -1<<sz + 1
		}
		t.fast[i] = v<<16 | int32(r)<<8 | int32(l+sz)
	}
	t.ok = true
	return nil
}

// bitReader legge i dati entropici già ripuliti dai byte di riempimento
// (0xFF 0x00 → 0xFF), con almeno 8 byte a zero in fondo al buffer.
type bitReader struct {
	buf []byte
	pos int    // prossimo byte da caricare
	acc uint64 // bit allineati a sinistra
	n   uint   // bit validi in acc
}

func (b *bitReader) reset(buf []byte, pos int) { b.buf, b.pos, b.acc, b.n = buf, pos, 0, 0 }

// refill porta i bit validi ad almeno 56. Nella via veloce carica 8 byte
// in una volta: i bit oltre n sono quelli veri dei byte successivi, quindi
// ricaricarli con un OR non li cambia.
func (b *bitReader) refill() {
	if b.pos+8 <= len(b.buf) {
		b.acc |= binary.BigEndian.Uint64(b.buf[b.pos:]) >> b.n
		b.pos += int(63-b.n) >> 3
		b.n |= 56
		return
	}
	for b.n <= 56 {
		var c byte
		if b.pos < len(b.buf) {
			c = b.buf[b.pos]
		}
		b.pos++
		b.acc |= uint64(c) << (56 - b.n)
		b.n += 8
	}
}

// decode legge un simbolo di Huffman; servono almeno 16 bit validi.
func (b *bitReader) decode(t *huffTable) (uint8, bool) {
	if e := t.lut[b.acc>>(64-lutBits)]; e != 0 {
		l := uint(e >> 8)
		b.acc <<= l
		b.n -= l
		return uint8(e), true
	}
	for l := uint(lutBits + 1); l <= 16; l++ {
		c := int32(b.acc >> (64 - l))
		if c <= t.maxcode[l] {
			b.acc <<= l
			b.n -= l
			return t.vals[uint8(c+t.delta[l])], true
		}
	}
	return 0, false
}

// receive legge s bit (1..16) e ne fa il valore con segno del JPEG.
func (b *bitReader) receive(s uint) int32 {
	if b.n < s {
		b.refill()
	}
	v := int32(b.acc >> (64 - s))
	b.acc <<= s
	b.n -= s
	if v < 1<<(s-1) {
		v += -1<<s + 1
	}
	return v
}

func (b *bitReader) skip(s uint) {
	if b.n < s {
		b.refill()
	}
	b.acc <<= s
	b.n -= s
}

// block decodifica un blocco nell'ordine naturale, già dequantizzato (blk
// deve essere a zero); restituisce l'ultimo indice zig-zag non nullo.
func (b *bitReader) block(dc, ac *huffTable, pred *int32, q *[64]int32, blk *[64]int32) (int, bool) {
	if b.n < 32 {
		b.refill()
	}
	s, ok := b.decode(dc)
	if !ok || s > 15 {
		return 0, false
	}
	if s > 0 {
		*pred += b.receive(uint(s))
	}
	blk[0] = *pred * q[0]
	last := 0
	for k := 1; k < 64; {
		if b.n < 32 {
			b.refill()
		}
		if f := ac.fast[b.acc>>(64-lutBits)]; f != 0 {
			tl := uint(f & 0xff)
			b.acc <<= tl
			b.n -= tl
			k += int(f >> 8 & 0xff)
			if k > 63 {
				return 0, false
			}
			blk[unzig[k]] = (f >> 16) * q[k]
			last = k
			k++
			continue
		}
		rs, ok := b.decode(ac)
		if !ok {
			return 0, false
		}
		r, s := int(rs>>4), uint(rs&15)
		if s == 0 {
			if r != 15 {
				break // fine del blocco
			}
			k += 16
			continue
		}
		k += r
		if k > 63 {
			return 0, false
		}
		blk[unzig[k]] = b.receive(s) * q[k]
		last = k
		k++
	}
	return last, true
}

// skipBlock legge un blocco senza conservarne i coefficienti.
func (b *bitReader) skipBlock(dc, ac *huffTable) bool {
	if b.n < 32 {
		b.refill()
	}
	s, ok := b.decode(dc)
	if !ok || s > 15 {
		return false
	}
	b.skip(uint(s))
	for k := 1; k < 64; {
		if b.n < 32 {
			b.refill()
		}
		if f := ac.fast[b.acc>>(64-lutBits)]; f != 0 {
			tl := uint(f & 0xff)
			b.acc <<= tl
			b.n -= tl
			k += int(f>>8&0xff) + 1
			continue
		}
		rs, ok := b.decode(ac)
		if !ok {
			return false
		}
		r, s := int(rs>>4), uint(rs&15)
		if s == 0 {
			if r != 15 {
				break
			}
			k += 16
			continue
		}
		k += r
		if k > 63 {
			return false
		}
		b.skip(s)
		k++
	}
	return true
}

// IDCT intera separabile, algoritmo LLM (Loeffler–Ligtenberg–Moschytz) con
// costanti a 13 bit: è la "islow" di libjpeg, precisa secondo IEEE 1180.
const (
	idctConstBits = 13
	idctPass1Bits = 2

	fix0_298631336 = 2446
	fix0_390180644 = 3196
	fix0_541196100 = 4433
	fix0_765366865 = 6270
	fix0_899976223 = 7373
	fix1_175875602 = 9633
	fix1_501321110 = 12299
	fix1_847759065 = 15137
	fix1_961570560 = 16069
	fix2_053119869 = 16819
	fix2_562915447 = 20995
	fix3_072711026 = 25172
)

func clamp8(v int) byte {
	if uint(v) > 255 {
		if v < 0 {
			return 0
		}
		return 255
	}
	return byte(v)
}

// idctPut trasforma blk (ordine naturale, dequantizzato), scrive i primi
// pw×ph pixel in dst con passo stride e rimette blk a zero. last è l'ultimo
// indice zig-zag non nullo: con il solo DC il blocco è uniforme.
func idctPut(blk *[64]int32, last int, dst []byte, stride, pw, ph int) {
	if last == 0 {
		v := clamp8((int(blk[0])+4)>>3 + 128)
		blk[0] = 0
		for y := 0; y < ph; y++ {
			row := dst[y*stride : y*stride+pw]
			for x := range row {
				row[x] = v
			}
		}
		return
	}
	var ws [64]int
	// colonne
	for c := 0; c < 8; c++ {
		if blk[8+c]|blk[16+c]|blk[24+c]|blk[32+c]|blk[40+c]|blk[48+c]|blk[56+c] == 0 {
			dc := int(blk[c]) << idctPass1Bits
			for r := 0; r < 64; r += 8 {
				ws[r+c] = dc
			}
			continue
		}
		z2, z3 := int(blk[16+c]), int(blk[48+c])
		z1 := (z2 + z3) * fix0_541196100
		tmp2 := z1 - z3*fix1_847759065
		tmp3 := z1 + z2*fix0_765366865
		z2, z3 = int(blk[c]), int(blk[32+c])
		tmp0 := (z2+z3)<<idctConstBits + 1<<(idctConstBits-idctPass1Bits-1)
		tmp1 := (z2-z3)<<idctConstBits + 1<<(idctConstBits-idctPass1Bits-1)
		tmp10, tmp13 := tmp0+tmp3, tmp0-tmp3
		tmp11, tmp12 := tmp1+tmp2, tmp1-tmp2

		t0, t1, t2, t3 := int(blk[56+c]), int(blk[40+c]), int(blk[24+c]), int(blk[8+c])
		z1, z2, z3, z4 := t0+t3, t1+t2, t0+t2, t1+t3
		z5 := (z3 + z4) * fix1_175875602
		t0 *= fix0_298631336
		t1 *= fix2_053119869
		t2 *= fix3_072711026
		t3 *= fix1_501321110
		z1 *= -fix0_899976223
		z2 *= -fix2_562915447
		z3 = z3*-fix1_961570560 + z5
		z4 = z4*-fix0_390180644 + z5
		t0 += z1 + z3
		t1 += z2 + z4
		t2 += z2 + z3
		t3 += z1 + z4

		const sh = idctConstBits - idctPass1Bits
		ws[c] = (tmp10 + t3) >> sh
		ws[56+c] = (tmp10 - t3) >> sh
		ws[8+c] = (tmp11 + t2) >> sh
		ws[48+c] = (tmp11 - t2) >> sh
		ws[16+c] = (tmp12 + t1) >> sh
		ws[40+c] = (tmp12 - t1) >> sh
		ws[24+c] = (tmp13 + t0) >> sh
		ws[32+c] = (tmp13 - t0) >> sh
	}
	*blk = [64]int32{}
	// righe: arrotondamento e +128 sono nel DC
	const sh = idctConstBits + idctPass1Bits + 3
	const bias = 1<<(idctPass1Bits+2) + 128<<(idctPass1Bits+3)
	var out [8]byte
	for y := 0; y < ph; y++ {
		w := ws[8*y : 8*y+8 : 8*y+8]
		row := dst[y*stride : y*stride+pw]
		if w[1]|w[2]|w[3]|w[4]|w[5]|w[6]|w[7] == 0 {
			v := clamp8((w[0] + bias) >> (idctPass1Bits + 3))
			for x := range row {
				row[x] = v
			}
			continue
		}
		z2, z3 := w[2], w[6]
		z1 := (z2 + z3) * fix0_541196100
		tmp2 := z1 - z3*fix1_847759065
		tmp3 := z1 + z2*fix0_765366865
		z2, z3 = w[0]+bias, w[4]
		tmp0 := (z2 + z3) << idctConstBits
		tmp1 := (z2 - z3) << idctConstBits
		tmp10, tmp13 := tmp0+tmp3, tmp0-tmp3
		tmp11, tmp12 := tmp1+tmp2, tmp1-tmp2

		t0, t1, t2, t3 := w[7], w[5], w[3], w[1]
		z1, z2, z3, z4 := t0+t3, t1+t2, t0+t2, t1+t3
		z5 := (z3 + z4) * fix1_175875602
		t0 *= fix0_298631336
		t1 *= fix2_053119869
		t2 *= fix3_072711026
		t3 *= fix1_501321110
		z1 *= -fix0_899976223
		z2 *= -fix2_562915447
		z3 = z3*-fix1_961570560 + z5
		z4 = z4*-fix0_390180644 + z5
		t0 += z1 + z3
		t1 += z2 + z4
		t2 += z2 + z3
		t3 += z1 + z4

		out[0] = clamp8((tmp10 + t3) >> sh)
		out[7] = clamp8((tmp10 - t3) >> sh)
		out[1] = clamp8((tmp11 + t2) >> sh)
		out[6] = clamp8((tmp11 - t2) >> sh)
		out[2] = clamp8((tmp12 + t1) >> sh)
		out[5] = clamp8((tmp12 - t1) >> sh)
		out[3] = clamp8((tmp13 + t0) >> sh)
		out[4] = clamp8((tmp13 - t0) >> sh)
		copy(row, out[:])
	}
}

// ---------------------------------------------------------------------------
// Frame

type scanComp struct {
	luma     bool // è la Y (prima componente del frame)
	comp     int  // indice della componente nel frame (0 Y, 1 Cb, 2 Cr)
	blocks   int  // blocchi per MCU
	dc, ac   *huffTable
	h, v, tq int // campionamento e tabella di quantizzazione (della Y)
}

type mjWorker struct {
	d   *mjpegDecoder
	run chan [2]int // tratti [da, a)
	br  bitReader
	blk [64]int32
}

// mjpegDecoder decodifica la Y di frame MJPEG di una dimensione nota. Non è
// rientrante: un frame alla volta.
type mjpegDecoder struct {
	qt     [4][64]int32 // ordine zig-zag
	dc, ac [4]huffTable

	w, h       int
	ri         int // MCU per tratto (0: nessun restart)
	mcuX, mcuY int // MCU per riga e per colonna
	mcuW, mcuH int // pixel
	sc         [3]scanComp
	ns         int

	buf    []byte  // dati entropici senza riempimento, più 8 byte a zero
	segs   []int32 // inizio di ogni tratto in buf, più la fine
	planes [3]plane

	workers []*mjWorker // goroutine oltre a quella del chiamante
	main    mjWorker
	wg      sync.WaitGroup
	failed  atomic.Bool

	lastSegs atomic.Int32 // tratti dell'ultimo frame, per l'interfaccia
}

// newMJPEGDecoder prepara un decoder che usa par goroutine (compresa
// quella del chiamante).
func newMJPEGDecoder(par int) *mjpegDecoder {
	d := &mjpegDecoder{}
	d.main.d = d
	for i := 1; i < max(par, 1); i++ {
		w := &mjWorker{d: d, run: make(chan [2]int, 1)}
		d.workers = append(d.workers, w)
		go w.loop()
	}
	return d
}

// Par è il numero di goroutine della decodifica.
func (d *mjpegDecoder) Par() int { return len(d.workers) + 1 }

// Close ferma le goroutine.
func (d *mjpegDecoder) Close() {
	for _, w := range d.workers {
		close(w.run)
	}
	d.workers = nil
}

func (w *mjWorker) loop() {
	for r := range w.run {
		w.d.decodeSegs(w, r[0], r[1])
		w.d.wg.Done()
	}
}

// plane è il piano di destinazione di una componente (dst nil: saltata).
type plane struct {
	dst  []byte
	w, h int
}

var errJPEGTruncated = errors.New("JPEG troncato")

// Decode scrive in dst (w×h, passo w) la luminanza del JPEG data.
func (d *mjpegDecoder) Decode(data []byte, w, h int, dst []byte) error {
	_, _, err := d.decode(data, w, h, dst, nil, nil)
	return err
}

// DecodeColor scrive la luminanza in y (w×h) e la crominanza in cb e cr,
// ciascuna di cw×ch (passo cw): con il 4:2:2 delle webcam cw = w/2, ch = h.
// cb e cr devono poter contenere w×h byte (il caso 4:4:4). Un JPEG in
// scala di grigi dà cw = ch = 0.
func (d *mjpegDecoder) DecodeColor(data []byte, w, h int, y, cb, cr []byte) (cw, ch int, err error) {
	if len(cb) < w*h || len(cr) < w*h {
		return 0, 0, fmt.Errorf("buffer della crominanza di %d e %d byte per %d×%d", len(cb), len(cr), w, h)
	}
	return d.decode(data, w, h, y, cb, cr)
}

func (d *mjpegDecoder) decode(data []byte, w, h int, dst, cb, cr []byte) (int, int, error) {
	if w <= 0 || h <= 0 || len(dst) < w*h {
		return 0, 0, fmt.Errorf("buffer di %d byte per %d×%d", len(dst), w, h)
	}
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, 0, errors.New("JPEG senza SOI")
	}
	for i := range d.dc {
		d.dc[i].ok, d.ac[i].ok = false, false
	}
	d.ri, d.w, d.h, d.ns = 0, 0, 0, 0
	var nf int
	var fcomp [3]struct {
		id     byte
		h, v   int
		tq     int
		inScan bool
	}
	hmax, vmax := 1, 1
	p := 2
	for {
		if p+4 > len(data) {
			return 0, 0, errJPEGTruncated
		}
		if data[p] != 0xff {
			return 0, 0, fmt.Errorf("JPEG: marker atteso all'offset %d", p)
		}
		m := data[p+1]
		if m == 0xff { // riempimento
			p++
			continue
		}
		p += 2
		if m == 0xd8 || m == 0x01 || (m >= 0xd0 && m <= 0xd7) {
			continue
		}
		if m == 0xd9 {
			return 0, 0, errors.New("JPEG senza scansione")
		}
		l := int(data[p])<<8 | int(data[p+1])
		if l < 2 || p+l > len(data) {
			return 0, 0, errJPEGTruncated
		}
		seg := data[p+2 : p+l]
		switch m {
		case 0xdb: // DQT
			for len(seg) > 0 {
				pq, tq := seg[0]>>4, int(seg[0]&15)
				if tq > 3 || pq > 1 {
					return 0, 0, errors.New("JPEG: tabella di quantizzazione non valida")
				}
				n := 64 << pq
				if len(seg) < 1+n {
					return 0, 0, errJPEGTruncated
				}
				for k := 0; k < 64; k++ {
					if pq == 0 {
						d.qt[tq][k] = int32(seg[1+k])
					} else {
						d.qt[tq][k] = int32(seg[1+2*k])<<8 | int32(seg[2+2*k])
					}
				}
				seg = seg[1+n:]
			}
		case 0xc4: // DHT
			for len(seg) > 0 {
				if len(seg) < 17 {
					return 0, 0, errJPEGTruncated
				}
				tc, th := seg[0]>>4, int(seg[0]&15)
				if tc > 1 || th > 3 {
					return 0, 0, errors.New("JPEG: tabella di Huffman non valida")
				}
				n := 0
				for _, c := range seg[1:17] {
					n += int(c)
				}
				if len(seg) < 17+n {
					return 0, 0, errJPEGTruncated
				}
				t := &d.dc[th]
				if tc == 1 {
					t = &d.ac[th]
				}
				if err := t.build(seg[1:17], seg[17:17+n]); err != nil {
					return 0, 0, err
				}
				seg = seg[17+n:]
			}
		case 0xdd: // DRI
			if len(seg) < 2 {
				return 0, 0, errJPEGTruncated
			}
			d.ri = int(seg[0])<<8 | int(seg[1])
		case 0xc0, 0xc1: // SOF baseline / sequenziale esteso
			if len(seg) < 6 || seg[0] != 8 {
				return 0, 0, errUnsupportedJPEG
			}
			d.h, d.w = int(seg[1])<<8|int(seg[2]), int(seg[3])<<8|int(seg[4])
			nf = int(seg[5])
			if nf != 1 && nf != 3 || len(seg) < 6+3*nf {
				return 0, 0, errUnsupportedJPEG
			}
			for i := 0; i < nf; i++ {
				c := &fcomp[i]
				c.id, c.h, c.v, c.tq = seg[6+3*i], int(seg[7+3*i]>>4), int(seg[7+3*i]&15), int(seg[8+3*i])
				if c.h < 1 || c.h > 4 || c.v < 1 || c.v > 4 || c.tq > 3 {
					return 0, 0, errors.New("JPEG: componente non valida")
				}
				hmax, vmax = max(hmax, c.h), max(vmax, c.v)
			}
		case 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
			return 0, 0, errUnsupportedJPEG // progressivo, senza perdite, aritmetico…
		case 0xda: // SOS
			if nf == 0 {
				return 0, 0, errors.New("JPEG: scansione prima del SOF")
			}
			if d.w != w || d.h != h {
				return 0, 0, fmt.Errorf("JPEG %d×%d invece di %d×%d", d.w, d.h, w, h)
			}
			if len(seg) < 1 {
				return 0, 0, errJPEGTruncated
			}
			ns := int(seg[0])
			if ns != nf || len(seg) < 1+2*ns+3 {
				return 0, 0, errUnsupportedJPEG // più scansioni
			}
			if ss, se, a := seg[1+2*ns], seg[2+2*ns], seg[3+2*ns]; ss != 0 || se != 63 || a != 0 {
				return 0, 0, errUnsupportedJPEG
			}
			lumaOK := false
			for i := 0; i < ns; i++ {
				id, tt := seg[1+2*i], seg[2+2*i]
				fi := -1
				for j := 0; j < nf; j++ {
					if fcomp[j].id == id && !fcomp[j].inScan {
						fi = j
					}
				}
				td, ta := int(tt>>4), int(tt&15)
				if fi < 0 || td > 3 || ta > 3 || !d.dc[td].ok || !d.ac[ta].ok {
					return 0, 0, errors.New("JPEG: scansione con componenti o tabelle mancanti")
				}
				fc := &fcomp[fi]
				fc.inScan = true
				sc := &d.sc[i]
				*sc = scanComp{luma: fi == 0, comp: fi, blocks: fc.h * fc.v, dc: &d.dc[td], ac: &d.ac[ta],
					h: fc.h, v: fc.v, tq: fc.tq}
				if ns == 1 {
					sc.blocks, sc.h, sc.v = 1, 1, 1
				}
				lumaOK = lumaOK || sc.luma
			}
			if !lumaOK {
				return 0, 0, errUnsupportedJPEG
			}
			d.ns = ns
			if ns == 1 {
				d.mcuW, d.mcuH = 8, 8
			} else {
				if fcomp[0].h != hmax || fcomp[0].v != vmax {
					return 0, 0, errUnsupportedJPEG // Y sottocampionata rispetto al colore
				}
				d.mcuW, d.mcuH = 8*hmax, 8*vmax
			}
			d.mcuX, d.mcuY = (w+d.mcuW-1)/d.mcuW, (h+d.mcuH-1)/d.mcuH
			return d.finish(data[p+l:], dst, cb, cr, &fcomp, nf, hmax, vmax)
		}
		p += l
	}
}

// finish prepara i piani di destinazione e decodifica la scansione.
func (d *mjpegDecoder) finish(ecs, dst, cb, cr []byte, fc *[3]struct {
	id     byte
	h, v   int
	tq     int
	inScan bool
}, nf, hmax, vmax int) (int, int, error) {
	d.planes = [3]plane{{dst: dst, w: d.w, h: d.h}}
	cw, ch := 0, 0
	if cb != nil && nf == 3 && d.ns == 3 && fc[1].h == fc[2].h && fc[1].v == fc[2].v {
		cw = (d.w*fc[1].h + hmax - 1) / hmax
		ch = (d.h*fc[1].v + vmax - 1) / vmax
		d.planes[1] = plane{dst: cb, w: cw, h: ch}
		d.planes[2] = plane{dst: cr, w: cw, h: ch}
	}
	if err := d.scan(ecs); err != nil {
		return 0, 0, err
	}
	return cw, ch, nil
}

// scan toglie i byte di riempimento, divide i dati ai restart e decodifica
// i tratti in parallelo.
func (d *mjpegDecoder) scan(ecs []byte) error {
	d.buf, d.segs = d.buf[:0], append(d.segs[:0], 0)
	end := false
	for i := 0; !end; {
		j := bytes.IndexByte(ecs[i:], 0xff)
		if j < 0 || i+j+1 >= len(ecs) {
			return errJPEGTruncated // nessun EOI
		}
		d.buf = append(d.buf, ecs[i:i+j]...)
		i += j
		switch m := ecs[i+1]; {
		case m == 0x00:
			d.buf = append(d.buf, 0xff)
			i += 2
		case m == 0xff:
			i++
		case m >= 0xd0 && m <= 0xd7:
			d.segs = append(d.segs, int32(len(d.buf)))
			i += 2
		default: // EOI o un altro marker: fine della scansione
			end = true
		}
	}
	d.segs = append(d.segs, int32(len(d.buf)))
	d.buf = append(d.buf, 0, 0, 0, 0, 0, 0, 0, 0)

	total := d.mcuX * d.mcuY
	want := 1
	if d.ri > 0 {
		want = (total + d.ri - 1) / d.ri
	}
	if len(d.segs)-1 < want {
		return fmt.Errorf("JPEG troncato: %d tratti su %d", len(d.segs)-1, want)
	}
	d.lastSegs.Store(int32(want))
	d.failed.Store(false)
	par := min(len(d.workers)+1, want)
	d.wg.Add(par - 1)
	for i := 1; i < par; i++ {
		d.workers[i-1].run <- [2]int{i * want / par, (i + 1) * want / par}
	}
	d.decodeSegs(&d.main, 0, want/par)
	d.wg.Wait()
	d.planes = [3]plane{}
	if d.failed.Load() {
		return errors.New("JPEG: dati di Huffman non validi")
	}
	return nil
}

// decodeSegs decodifica i tratti [s0, s1).
func (d *mjpegDecoder) decodeSegs(wk *mjWorker, s0, s1 int) {
	total := d.mcuX * d.mcuY
	per := d.ri
	if per == 0 {
		per = total
	}
	br := &wk.br
	for s := s0; s < s1; s++ {
		start := int(d.segs[s])
		br.reset(d.buf, start)
		m1 := min((s+1)*per, total)
		if !d.decodeMCUs(wk, s*per, m1) {
			d.failed.Store(true)
			return
		}
		// i bit consumati non devono sconfinare nel tratto successivo
		if (br.pos-start)*8-int(br.n) > (int(d.segs[s+1])-start)*8 {
			d.failed.Store(true)
			return
		}
	}
}

func (d *mjpegDecoder) decodeMCUs(wk *mjWorker, m0, m1 int) bool {
	br, blk := &wk.br, &wk.blk
	var pred [3]int32
	for m := m0; m < m1; m++ {
		mx, my := m%d.mcuX, m/d.mcuX
		for i := 0; i < d.ns; i++ {
			sc := &d.sc[i]
			pl := &d.planes[sc.comp]
			if pl.dst == nil {
				for k := 0; k < sc.blocks; k++ {
					if !br.skipBlock(sc.dc, sc.ac) {
						return false
					}
				}
				continue
			}
			q := &d.qt[sc.tq]
			for by := 0; by < sc.v; by++ {
				for bx := 0; bx < sc.h; bx++ {
					last, ok := br.block(sc.dc, sc.ac, &pred[i], q, blk)
					if !ok {
						*blk = [64]int32{}
						return false
					}
					px, py := (mx*sc.h+bx)*8, (my*sc.v+by)*8
					if px >= pl.w || py >= pl.h {
						*blk = [64]int32{} // blocco di riempimento oltre il bordo
						continue
					}
					idctPut(blk, last, pl.dst[py*pl.w+px:], pl.w, min(8, pl.w-px), min(8, pl.h-py))
				}
			}
		}
	}
	return true
}
