package tracker

import (
	"encoding/binary"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

type fakeHeadPose struct {
	offers   [][]byte
	observed int
	serial   string
	area     *ttp.DisplayArea
}

func (f *fakeHeadPose) OfferImage(p []byte, rx int64) {
	f.offers = append(f.offers, append([]byte(nil), p...))
}
func (f *fakeHeadPose) Observe(s *gaze.Sample) {
	f.observed++
	s.Head.RotationRad[0], s.Head.RotationValid[0] = 0.25, true
}
func (f *fakeHeadPose) SetDevice(serial string, a *ttp.DisplayArea) { f.serial, f.area = serial, a }

// streamMsg costruisce un messaggio device→host di uno stream.
func streamMsg(op uint32, payload []byte) []byte {
	m := make([]byte, ttp.FullHeader, ttp.FullHeader+len(payload))
	m[0] = 0x01
	binary.LittleEndian.PutUint32(m[4:], uint32(ttp.FullHeader+len(payload)))
	be := binary.BigEndian
	be.PutUint32(m[8:], ttp.MarkerStream)
	be.PutUint32(m[20:], op)
	be.PutUint32(m[28:], uint32(len(payload)))
	return append(m, payload...)
}

func TestImagesRoutedToHeadPose(t *testing.T) {
	hp := &fakeHeadPose{}
	tr := New(Options{HeadPose: hp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !slices.Contains(tr.secondaryStreams(), ttp.StreamImage) {
		t.Fatal("image stream not subscribed")
	}
	s := &Session{t: tr, log: tr.log}
	s.route(streamMsg(ttp.StreamImage, []byte{1, 2, 3}), 7)
	s.route(streamMsg(0x1772, []byte{9}), 8) // log: non va allo stimatore
	if len(hp.offers) != 1 || string(hp.offers[0]) != "\x01\x02\x03" {
		t.Fatalf("offers %v", hp.offers)
	}
	if tr.Stats.OtherStream.Load() != 2 {
		t.Fatalf("other streams %d", tr.Stats.OtherStream.Load())
	}
}

// L'emitter passa ogni campione allo stimatore prima dei sink.
func TestEmitterObservesBeforeSinks(t *testing.T) {
	hp := &fakeHeadPose{}
	var got []gaze.Sample
	sk := sinkFunc(func(s *gaze.Sample) { got = append(got, *s) })
	tr := New(Options{HeadPose: hp, FramePool: 4}, slog.New(slog.NewTextHandler(io.Discard, nil)), sk)
	s := &Session{t: tr, log: tr.log, firstSeq: 1,
		frameFree: make(chan *frameBuf, 4), results: make(chan result, 4)}
	s.results <- result{s: gaze.Sample{Seq: 1}, ok: true, fb: &frameBuf{}}
	s.results <- result{s: gaze.Sample{Seq: 2}, ok: true, fb: &frameBuf{}}
	close(s.results)
	s.emitter()
	if hp.observed != 2 || len(got) != 2 || !got[0].Head.RotationValid[0] || got[1].Head.RotationRad[0] != 0.25 {
		t.Fatalf("observed %d, sink got %+v", hp.observed, got)
	}
}

type sinkFunc func(s *gaze.Sample)

func (f sinkFunc) Write(s *gaze.Sample) error { f(s); return nil }
func (f sinkFunc) Close() error               { return nil }

func TestImageGate(t *testing.T) {
	t0 := time.Unix(1000, 0)
	g := imageGate{on: true}
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	if g.presence(gaze.PresenceAway, at(0)) || g.due(at(2900)) {
		t.Fatal("paused too early")
	}
	// un ritorno breve annulla la sospensione programmata
	if g.presence(gaze.PresencePresent, at(2950)) {
		t.Fatal("stream is on: nothing to resume")
	}
	g.presence(gaze.PresenceAway, at(3000))
	if g.due(at(5900)) {
		t.Fatal("pause not restarted after a short return")
	}
	if !g.due(at(6000)) {
		t.Fatal("not paused after 3 s away")
	}
	g.on = false
	if g.due(at(9000)) {
		t.Fatal("paused twice")
	}
	if !g.presence(gaze.PresencePresent, at(9100)) {
		t.Fatal("not resumed when the user is back")
	}
	g.on = true
	// presenza sconosciuta (stream presence rifiutato): mai sospendere
	g2 := imageGate{on: true}
	g2.presence(gaze.PresenceUnknown, at(0))
	if g2.due(at(60000)) {
		t.Fatal("paused with unknown presence")
	}
}
