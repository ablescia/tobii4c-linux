package gaze

import (
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

func realRow(t testing.TB) *ttp.Row {
	t.Helper()
	raw, err := os.ReadFile("../ttp/testdata/et5-session1-gaze-frame.hex")
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := hex.DecodeString(strings.TrimSpace(string(raw)))
	_, payload, err := ttp.ParseHeader(msg)
	if err != nil {
		t.Fatal(err)
	}
	var row ttp.Row
	if err := ttp.DecodeRow(payload, &row); err != nil {
		t.Fatal(err)
	}
	return &row
}

func TestDecodeRealFrame(t *testing.T) {
	row := realRow(t)
	d := NewDecoder(DefaultKeyMap())
	s := Sample{Seq: 7, HostTimeNs: 42}
	d.Decode(row, &s)

	if s.Seq != 7 || s.HostTimeNs != 42 {
		t.Fatal("host fields overwritten")
	}
	if s.DeviceTimeUs != 9_613_320_391 || !s.HasCounter || s.FrameCounter != 43_780 {
		t.Fatalf("time/counter %+v", s)
	}
	if !s.GazeValid || s.Gaze != [2]float32{0.5872913599014282, 0.09927384555339813} {
		t.Fatalf("gaze %v valid=%v", s.Gaze, s.GazeValid)
	}
	if !s.Left.Tracked || !s.Right.Tracked || !s.Left.OriginValid || !s.Right.OriginValid {
		t.Fatalf("eyes %+v / %+v", s.Left, s.Right)
	}
	if !s.Left.PupilValid || s.Left.PupilMM < 1 || s.Left.PupilMM > 9 {
		t.Fatalf("pupil %v", s.Left.PupilMM)
	}
	ipd := math.Sqrt(sq(s.Right.OriginMM[0]-s.Left.OriginMM[0]) +
		sq(s.Right.OriginMM[1]-s.Left.OriginMM[1]) + sq(s.Right.OriginMM[2]-s.Left.OriginMM[2]))
	if ipd < 50 || ipd > 80 {
		t.Fatalf("implausible inter-pupillary distance %.1f mm", ipd)
	}
	h := s.Head
	if h.Source != HeadDerived || !h.PositionValid || h.PositionMM[2] < 400 || h.PositionMM[2] > 900 {
		t.Fatalf("head %+v", h)
	}
	if h.RotationValid[0] || !h.RotationValid[1] || math.Abs(float64(h.RotationRad[1])) > 0.6 {
		t.Fatalf("head rotation %+v", h)
	}
}

func TestInvalidEyeIsNotTrusted(t *testing.T) {
	row := realRow(t)
	// Forza lo status dell'occhio sinistro a 4 (non rilevato).
	for i := 0; i < row.N; i++ {
		if row.Cols[i].Key == 0x07 {
			row.Cols[i].Value.U = 4
		}
	}
	var s Sample
	NewDecoder(DefaultKeyMap()).Decode(row, &s)
	if s.Left.Tracked || s.Left.OriginValid || s.Left.PupilValid || s.Head.PositionValid {
		t.Fatalf("left eye must be invalid: %+v head %+v", s.Left, s.Head)
	}
	if !s.Right.OriginValid {
		t.Fatal("right eye must stay valid")
	}
}

func TestDecodeZeroAlloc(t *testing.T) {
	row := realRow(t)
	d := NewDecoder(DefaultKeyMap())
	var s Sample
	if a := testing.AllocsPerRun(1000, func() { d.Decode(row, &s) }); a != 0 {
		t.Fatalf("Decode allocates %.1f", a)
	}
}

func sq(f float32) float64 { return float64(f) * float64(f) }

func setCol(t *testing.T, row *ttp.Row, key uint32, f func(v *ttp.Value)) {
	t.Helper()
	for i := 0; i < row.N; i++ {
		if row.Cols[i].Key == key {
			f(&row.Cols[i].Value)
			return
		}
	}
	t.Fatalf("colonna 0x%02x assente", key)
}

// Nel frame ET5 reale ogni nuova colonna ha un valore noto: centro corneale
// nel sistema dello schermo (0x22/0x24), track box (0x03/0x09) con la sua
// validità (0x1d/0x1e), apertura non disponibile (−1, validità 0).
func TestDecodeDisplayFrameTrackBoxOpenness(t *testing.T) {
	row := realRow(t)
	var s Sample
	NewDecoder(DefaultKeyMap()).Decode(row, &s)

	for _, c := range []struct {
		e    Eye
		disp uint32
		tb   uint32
	}{{s.Left, 0x22, 0x03}, {s.Right, 0x24, 0x09}} {
		want, _ := row.Get(c.disp)
		if !c.e.OriginDisplayValid || c.e.OriginDisplayMM != f3(want) {
			t.Errorf("origin display %v valid=%v, colonna 0x%02x = %v", c.e.OriginDisplayMM, c.e.OriginDisplayValid, c.disp, want.F)
		}
		tb, _ := row.Get(c.tb)
		if !c.e.TrackBoxValid || c.e.TrackBox != f3(tb) {
			t.Errorf("trackbox %v valid=%v", c.e.TrackBox, c.e.TrackBoxValid)
		}
		if !c.e.Gaze2DValid {
			t.Error("gaze 2D di un occhio tracciato deve essere valido")
		}
		if c.e.OpennessValid || c.e.OpennessMM != -1 {
			t.Errorf("openness %v valid=%v: sull'ET5 vale -1, non valida", c.e.OpennessMM, c.e.OpennessValid)
		}
	}

	// La testa è nel sistema dello schermo: punto medio dei due 0x22/0x24.
	h := s.Head
	for i := 0; i < 3; i++ {
		mid := (s.Left.OriginDisplayMM[i] + s.Right.OriginDisplayMM[i]) / 2
		if h.PositionMM[i] != mid {
			t.Fatalf("head[%d] %v, punto medio display %v", i, h.PositionMM[i], mid)
		}
	}
	// Lo schermo è più in alto e inclinato rispetto al tracker: le due
	// posizioni differiscono di decine di mm in y e z.
	trk, ok := s.EyesTrackerMM()
	if !ok || math.Abs(float64(h.PositionMM[1]-trk[1])) < 20 || math.Abs(float64(h.PositionMM[2]-trk[2])) < 20 {
		t.Fatalf("head (schermo) %v troppo vicina al punto medio nel tracker %v", h.PositionMM, trk)
	}
}

func TestTrackBoxValidityColumn(t *testing.T) {
	row := realRow(t)
	setCol(t, row, 0x1d, func(v *ttp.Value) { v.U = 0 })
	var s Sample
	NewDecoder(DefaultKeyMap()).Decode(row, &s)
	if s.Left.TrackBoxValid || !s.Left.Tracked || !s.Left.Gaze2DValid {
		t.Fatalf("0x1d=0 invalida solo il track box: %+v", s.Left)
	}
	if !s.Right.TrackBoxValid {
		t.Fatal("il track box destro resta valido")
	}
}

// Una mappa salvata prima di questa versione aveva gaze_2d_valid=0x1d: la
// chiave ora appartiene al track box e lo sguardo 2D torna a dipendere da Tracked.
func TestLegacyKeyMapGaze2DValid(t *testing.T) {
	km := DefaultKeyMap()
	km.Left.Gaze2DValid, km.Right.Gaze2DValid = 0x1d, 0x1e
	row := realRow(t)
	setCol(t, row, 0x1d, func(v *ttp.Value) { v.U = 0 })
	var s Sample
	NewDecoder(km).Decode(row, &s)
	if !s.Left.Gaze2DValid || s.Left.TrackBoxValid {
		t.Fatalf("gaze2d=%v trackbox=%v", s.Left.Gaze2DValid, s.Left.TrackBoxValid)
	}
}

func TestOpennessUsesFirmwareValidity(t *testing.T) {
	row := realRow(t)
	setCol(t, row, 0x29, func(v *ttp.Value) { v.F[0] = 10.5 })
	setCol(t, row, 0x2a, func(v *ttp.Value) { v.U = 1 })
	setCol(t, row, 0x07, func(v *ttp.Value) { v.U = 4 }) // occhio sinistro non tracciato
	var s Sample
	NewDecoder(DefaultKeyMap()).Decode(row, &s)
	if !s.Left.OpennessValid || s.Left.OpennessMM != 10.5 {
		t.Fatalf("openness %v valid=%v: deve seguire 0x2a anche con l'occhio non tracciato", s.Left.OpennessMM, s.Left.OpennessValid)
	}
	if s.Left.OriginDisplayValid || s.Left.TrackBoxValid || s.Head.PositionValid {
		t.Fatalf("occhio non tracciato: %+v head %+v", s.Left, s.Head)
	}
	if s.Right.OpennessValid {
		t.Fatal("openness destra: validità 0 nel frame")
	}
}

func TestDecodePresence(t *testing.T) {
	raw, err := os.ReadFile("../ttp/testdata/et5-init-presence.hex")
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := hex.DecodeString(strings.TrimSpace(string(raw)))
	h, payload, err := ttp.ParseHeader(msg)
	if err != nil || h.Op != ttp.StreamPresence {
		t.Fatalf("header %+v err %v", h, err)
	}
	var row ttp.Row
	if err := ttp.DecodeRow(payload, &row); err != nil {
		t.Fatal(err)
	}
	p, us, ok := DecodePresence(&row)
	if !ok || p != PresencePresent || us != 105_523_788_040 {
		t.Fatalf("presence %v us=%d ok=%v", p, us, ok)
	}
	for raw, want := range map[uint32]Presence{0: PresenceUnknown, 1: PresenceAway, 2: PresencePresent, 7: PresenceUnknown} {
		setCol(t, &row, PresenceKeyValue, func(v *ttp.Value) { v.U = raw })
		if p, _, _ := DecodePresence(&row); p != want {
			t.Errorf("valore %d → %v, atteso %v", raw, p, want)
		}
	}
}

func TestDecodeKeepsPresence(t *testing.T) {
	row := realRow(t)
	s := Sample{Presence: PresenceAway, PresenceUs: 123}
	NewDecoder(DefaultKeyMap()).Decode(row, &s)
	if s.Presence != PresenceAway || s.PresenceUs != 123 {
		t.Fatalf("presence sovrascritta: %v %d", s.Presence, s.PresenceUs)
	}
}
