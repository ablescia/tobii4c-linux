// Package calib implementa l'app di configurazione del Tobii 4C, sul modello
// dell'applicazione ufficiale Tobii: posizionamento del sensore (display
// area), posizione dell'utente nel campo visivo, calibrazione a 7 punti dello
// sguardo e verifica dell'accuratezza.
//
// Il server HTTP ascolta solo su loopback e serve una pagina incorporata che
// va a schermo intero sul monitor col tracker. La logica temporale
// (attesa della fissazione, raccolta, verifica) gira nel server, che vede lo
// stream a 90 Hz. Il browser si limita a disegnare.
package calib

import (
	"sync"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

// hubSize copre ~5,7 s a 90 Hz.
const hubSize = 512

// Hub è un tracker.Sink che conserva gli ultimi campioni in un ring buffer.
// Write è chiamato dalla goroutine emitter. Le letture arrivano dagli handler HTTP.
type Hub struct {
	mu   sync.Mutex
	ring [hubSize]gaze.Sample
	n    uint64
}

// Write memorizza il campione (nessuna allocazione).
func (h *Hub) Write(s *gaze.Sample) error {
	h.mu.Lock()
	h.ring[h.n%hubSize] = *s
	h.n++
	h.mu.Unlock()
	return nil
}

// Close non fa nulla.
func (h *Hub) Close() error { return nil }

// Latest restituisce l'ultimo campione.
func (h *Hub) Latest() (gaze.Sample, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n == 0 {
		return gaze.Sample{}, false
	}
	return h.ring[(h.n-1)%hubSize], true
}

// Since accoda a dst i campioni ricevuti a partire da sinceNs (HostTimeNs).
func (h *Hub) Since(sinceNs int64, dst []gaze.Sample) []gaze.Sample {
	h.mu.Lock()
	defer h.mu.Unlock()
	start := uint64(0)
	if h.n > hubSize {
		start = h.n - hubSize
	}
	for i := start; i < h.n; i++ {
		s := &h.ring[i%hubSize]
		if s.HostTimeNs >= sinceNs {
			dst = append(dst, *s)
		}
	}
	return dst
}
