package telemetry

import (
	"slices"
	"sync/atomic"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
)

// counters sono i contatori cumulativi della pipeline (tracker.Stats).
type counters struct {
	gaze, hostDrops, devDrops, devPauses, decodeErr, resyncs          uint64
	usbTransfers, usbBytes, orphan, notes, sinkErr, reconnects, other uint64
	samples                                                           uint64
}

func (a *App) counters() counters {
	c := counters{samples: a.snap.Samples}
	st := a.Col.Stats
	if st == nil {
		c.gaze = a.snap.Samples
		return c
	}
	c.gaze = st.GazeFrames.Load()
	c.hostDrops = st.HostDrops.Load()
	c.devDrops = st.DeviceDrops.Load()
	c.devPauses = st.DevicePauses.Load()
	c.decodeErr = st.DecodeErrors.Load()
	c.resyncs = st.Resyncs.Load()
	c.usbTransfers = st.UsbTransfers.Load()
	c.usbBytes = st.UsbBytes.Load()
	c.orphan = st.OrphanReplies.Load()
	c.notes = st.Notifications.Load()
	c.sinkErr = st.SinkErrors.Load()
	c.reconnects = st.Reconnects.Load()
	c.other = st.OtherStream.Load()
	return c
}

// since restituisce il contatore al netto dell'azzeramento (tasto R).
func since(v, base uint64) uint64 {
	if v < base {
		return v
	}
	return v - base
}

// rates sono le grandezze calcolate una volta al secondo.
type rates struct {
	at       time.Time
	prev     counters
	prevStrm map[uint32]uint64
	prevHP   headpose.Stats

	gazeHz, usbPerS, usbBps float64
	streamHz                map[uint32]float64

	hp                   headpose.Stats // cumulative, dall'ultima lettura
	hpImgHz, hpProcHz    float64
	hpDropHz, hpSearchHz float64
	trackedPct           float64
	diff                 headpose.Stats // ultimo intervallo con confronti immagine/occhi
	haveDiff             bool
	coverage             headpose.Coverage
	lat50, lat99, latMax int64 // ns
	ka                   int64 // RTT del keep-alive, µs
	statusBusy           atomic.Bool
}

func (r *rates) update(a *App, now time.Time) {
	dt := now.Sub(r.at).Seconds()
	first := r.at.IsZero()
	r.at = now
	cur := a.counters()
	if !first && dt > 0 {
		r.gazeHz = float64(cur.gaze-r.prev.gaze) / dt
		r.usbPerS = float64(cur.usbTransfers-r.prev.usbTransfers) / dt
		r.usbBps = float64(cur.usbBytes-r.prev.usbBytes) / dt
	}
	r.prev = cur
	if r.streamHz == nil {
		r.streamHz, r.prevStrm = map[uint32]float64{}, map[uint32]uint64{}
	}
	for id, st := range a.snap.Streams {
		if p, ok := r.prevStrm[id]; ok && dt > 0 {
			r.streamHz[id] = float64(st.Count-p) / dt
		}
		r.prevStrm[id] = st.Count
	}
	if st := a.Col.Stats; st != nil {
		r.ka = st.LastKeepAliveU.Load()
	}

	lat := slices.Clone(a.snap.Latency)
	if len(lat) > 0 {
		slices.Sort(lat)
		r.lat50, r.lat99, r.latMax = lat[len(lat)/2], lat[len(lat)*99/100], lat[len(lat)-1]
	}

	if hp := a.Col.HP; hp != nil {
		st := hp.TakeStats()
		p := r.prevHP
		if !first && dt > 0 && st.Frames >= p.Frames {
			r.hpImgHz = float64(st.Frames-p.Frames) / dt
			r.hpProcHz = float64(st.Processed-p.Processed) / dt
			r.hpDropHz = float64(st.Dropped-p.Dropped) / dt
			r.hpSearchHz = float64(st.Searches-p.Searches) / dt
			if done := st.Processed - p.Processed; done > 0 {
				r.trackedPct = 100 * float64(st.Tracked-p.Tracked) / float64(done)
			}
		}
		r.prevHP, r.hp = st, st
		if st.Compared > 0 {
			r.diff, r.haveDiff = st, true
		}
		r.coverage = hp.Coverage()
		// Status attende la goroutine del modello: fuori dal loop.
		if r.statusBusy.CompareAndSwap(false, true) {
			go func() {
				defer r.statusBusy.Store(false)
				a.Col.SetHeadPoseStatus(hp.Status())
			}()
		}
	}
}
