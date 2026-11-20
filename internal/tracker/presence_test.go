package tracker

import (
	enchex "encoding/hex"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

func fixtureMsg(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../ttp/testdata/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := enchex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// La presenza vale per i frame gaze che arrivano DOPO il messaggio 0x504,
// nell'ordine dell'USB, anche se i worker decodificano in parallelo.
func TestPresenceStampedOnFollowingFrames(t *testing.T) {
	tr := New(Options{Presence: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &Session{t: tr, log: tr.log,
		frameFree: make(chan *frameBuf, 2), jobs: make(chan job, 2), results: make(chan result, 2)}
	s.frameFree <- &frameBuf{}
	s.frameFree <- &frameBuf{}
	gazeMsg, presMsg := fixtureMsg(t, "et5-session1-gaze-frame"), fixtureMsg(t, "et5-init-presence")

	s.route(gazeMsg, 1)
	s.route(presMsg, 2)
	s.route(gazeMsg, 3)
	close(s.jobs)
	s.worker()
	close(s.results)

	var got []gaze.Sample
	for r := range s.results {
		if !r.ok {
			t.Fatal("frame non decodificato")
		}
		got = append(got, r.s)
	}
	slices.SortFunc(got, func(a, b gaze.Sample) int { return int(a.Seq) - int(b.Seq) })
	if len(got) != 2 {
		t.Fatalf("%d campioni", len(got))
	}
	if got[0].Presence != gaze.PresenceUnknown || got[0].PresenceUs != 0 {
		t.Errorf("primo frame: %v %d, atteso unknown", got[0].Presence, got[0].PresenceUs)
	}
	if got[1].Presence != gaze.PresencePresent || got[1].PresenceUs != 105_523_788_040 {
		t.Errorf("secondo frame: %v %d, atteso present", got[1].Presence, got[1].PresenceUs)
	}
	if !got[1].Head.PositionValid {
		t.Error("la presenza non deve alterare la decodifica dello sguardo")
	}
	if tr.Stats.OtherStream.Load() != 1 || tr.Stats.DecodeErrors.Load() != 0 {
		t.Errorf("stats other=%d decode_errors=%d", tr.Stats.OtherStream.Load(), tr.Stats.DecodeErrors.Load())
	}
}

func TestSecondaryStreams(t *testing.T) {
	cases := []struct {
		opt  Options
		want []uint32
	}{
		{Options{}, nil},
		{Options{Presence: true}, []uint32{ttp.StreamPresence}},
		{Options{Presence: true, HeadPose: &fakeHeadPose{}}, []uint32{ttp.StreamPresence, ttp.StreamImage}},
		{Options{HeadPose: &fakeHeadPose{}}, []uint32{ttp.StreamImage}},
	}
	for _, c := range cases {
		tr := &Tracker{opt: c.opt}
		if got := tr.secondaryStreams(); !slices.Equal(got, c.want) {
			t.Errorf("%+v: %v, atteso %v", c.opt, got, c.want)
		}
	}
}
