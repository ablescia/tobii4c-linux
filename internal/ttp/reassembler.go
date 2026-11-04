package ttp

import "encoding/binary"

const (
	// MaxMessageLen è il limite di plausibilità di un messaggio device→host
	// (un'immagine NIR dell'ET5 è ~78 KB). Oltre si considera persa la sincronia.
	MaxMessageLen = 1 << 20
	// maxContinuation limita una singola continuazione di risposta frammentata
	// (un blob di calibrazione arriva fino a ~660 KB).
	maxContinuation = 4 << 20
)

// Reassembler ricompone messaggi TTP completi a partire dalle letture bulk
// grezze dell'endpoint IN 0x83.
//
// Di norma una bulk transfer contiene esattamente un messaggio (ogni messaggio
// termina con un short packet), ma un buffer più piccolo del messaggio, uno
// stallo o un messaggio lungo multiplo di 512 B possono spezzarlo o
// accorparlo: il riassemblatore cuce per lunghezza dichiarata e risincronizza
// scartando byte quando il prefisso non è plausibile.
//
// Le risposte molto grandi arrivano "a pezzi": un normale messaggio 0x52 il cui
// header dichiara più payload di quanto ne trasporti, seguito da continuazioni
// che hanno l'envelope ma non l'header. Vengono riattaccate alla testa e la
// risposta viene emessa intera.
//
// Non è safe per uso concorrente: lo usa solo la goroutine framer.
// In regime stazionario non alloca: il buffer interno cresce una volta sola.
type Reassembler struct {
	buf   []byte
	start int

	cont      []byte // testa di una risposta frammentata in corso
	remaining int

	// Statistiche (lette solo dalla goroutine proprietaria o dopo il join).
	SkippedBytes uint64 // byte scartati per risincronizzare
	Resyncs      uint64
}

// NewReassembler crea un riassemblatore con capacità iniziale capHint.
func NewReassembler(capHint int) *Reassembler {
	return &Reassembler{buf: make([]byte, 0, capHint)}
}

// Push accoda una lettura bulk e invoca emit per ogni messaggio completo, in
// ordine. Lo slice passato a emit è valido SOLO durante la callback: chi deve
// conservarlo lo copia.
func (r *Reassembler) Push(data []byte, emit func(msg []byte)) {
	if len(data) == 0 {
		return
	}
	r.compact(len(data))
	r.buf = append(r.buf, data...)

	for {
		p := r.buf[r.start:]
		if len(p) == 0 {
			r.buf, r.start = r.buf[:0], 0
			return
		}
		if r.cont != nil && looksLikeContinuation(p) && (len(p) < 12 || !knownMarker(p)) {
			if !r.takeContinuation(emit) {
				return
			}
			continue
		}
		if len(p) < 12 {
			return
		}
		if !looksLikePrefix(p) {
			r.resync()
			continue
		}
		total := int(binary.LittleEndian.Uint32(p[4:]))
		if len(p) < total {
			return // attende il resto
		}
		msg := p[:total]
		r.start += total
		if missing := missingPayload(msg); missing > 0 {
			// Testa di una risposta frammentata: la copia vive oltre la callback.
			r.cont = append(make([]byte, 0, len(msg)+missing), msg...)
			r.remaining = missing
			continue
		}
		emit(msg)
	}
}

// compact fa spazio per n byte riportando i dati pendenti a inizio buffer.
func (r *Reassembler) compact(n int) {
	if r.start == 0 {
		return
	}
	if r.start == len(r.buf) {
		r.buf, r.start = r.buf[:0], 0
		return
	}
	if len(r.buf)+n > cap(r.buf) || r.start > cap(r.buf)/2 {
		k := copy(r.buf, r.buf[r.start:])
		r.buf, r.start = r.buf[:k], 0
	}
}

// resync avanza fino al prossimo prefisso plausibile (o svuota).
func (r *Reassembler) resync() {
	p := r.buf[r.start:]
	r.Resyncs++
	for i := 1; i+12 <= len(p); i++ {
		if looksLikePrefix(p[i:]) {
			r.start += i
			r.SkippedBytes += uint64(i)
			return
		}
	}
	// Nessun prefisso trovato: tieni solo gli ultimi 11 byte, che potrebbero
	// essere l'inizio di un prefisso spezzato. Il chiamante è sempre entrato
	// con ≥12 byte, quindi qui si avanza di almeno 1 e il loop termina.
	const keep = 11
	r.SkippedBytes += uint64(len(p) - keep)
	r.start += len(p) - keep
}

func (r *Reassembler) takeContinuation(emit func([]byte)) bool {
	p := r.buf[r.start:]
	total := int(binary.LittleEndian.Uint32(p[4:]))
	if len(p) < total {
		return false
	}
	body := p[EnvelopeSize:total]
	r.start += total
	if len(body) > r.remaining {
		// Continuazione più lunga del dichiarato: risposta corrotta, scartala.
		r.cont, r.remaining = nil, 0
		return true
	}
	r.cont = append(r.cont, body...)
	r.remaining -= len(body)
	if r.remaining == 0 {
		msg := r.cont
		binary.LittleEndian.PutUint32(msg[4:], uint32(len(msg)))
		r.cont = nil
		emit(msg)
	}
	return true
}

func knownMarker(p []byte) bool {
	switch binary.BigEndian.Uint32(p[8:]) {
	case MarkerRequest, MarkerResponse, MarkerStream, MarkerNotification:
		return true
	}
	return false
}

func looksLikePrefix(p []byte) bool {
	if len(p) < 12 || p[0] != dirIn || p[1] != 0 || p[2] != 0 || p[3] != 0 {
		return false
	}
	n := binary.LittleEndian.Uint32(p[4:])
	return n >= FullHeader && n <= MaxMessageLen && knownMarker(p)
}

func looksLikeContinuation(p []byte) bool {
	if len(p) < 8 || p[0] != dirIn || p[1] != 0 || p[2] != 0 || p[3] != 0 {
		return false
	}
	n := binary.LittleEndian.Uint32(p[4:])
	return n >= 9 && n <= maxContinuation
}

// missingPayload: quanti byte di payload mancano a una risposta frammentata.
func missingPayload(msg []byte) int {
	if binary.BigEndian.Uint32(msg[8:]) != MarkerResponse {
		return 0
	}
	declared := int(binary.BigEndian.Uint32(msg[28:]))
	have := len(msg) - FullHeader
	if declared > have {
		return declared - have
	}
	return 0
}
