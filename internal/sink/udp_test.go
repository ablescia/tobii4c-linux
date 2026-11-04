package sink

import (
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

func sample() gaze.Sample {
	return gaze.Sample{
		Seq: 99, HostTimeNs: 1_700_000_000_123_456_789, DeviceTimeUs: 9_613_320_391,
		Gaze: [2]float32{0.58, 0.09}, GazeValid: true, GazeRaw: [2]float32{0.6, 0.1},
		Left: gaze.Eye{Tracked: true, OriginValid: true, OriginMM: [3]float32{-30, 100, 600}, PupilMM: 3.5, PupilValid: true,
			Gaze2D: [2]float32{0.5, 0.1}, Gaze2DValid: true,
			OriginDisplayMM: [3]float32{-31, 151, 545}, OriginDisplayValid: true,
			TrackBox: [3]float32{0.61, 0.38, 0.56}, TrackBoxValid: true,
			OpennessMM: 10.25, OpennessValid: true},
		Right: gaze.Eye{Tracked: true, OriginValid: true, OriginMM: [3]float32{33, 101, 605}, PupilMM: 3.4, PupilValid: true,
			OriginDisplayMM: [3]float32{32, 150, 551}, TrackBox: [3]float32{0.47, 0.39, 0.55}, OpennessMM: -1},
		Head: gaze.Head{Source: gaze.HeadDerived, PositionMM: [3]float32{0.5, 150.5, 548}, PositionValid: true,
			RotationRad: [3]float32{0, -0.08, 0.016}, RotationValid: [3]bool{false, true, true}},
		Presence: gaze.PresencePresent, PresenceUs: 9_613_000_000,
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	in := sample()
	var p [PacketSize]byte
	Marshal(&p, &in)
	var out gaze.Sample
	if err := Unmarshal(p[:], &out); err != nil {
		t.Fatal(err)
	}
	// Campi non trasportati dal pacchetto.
	out.FrameCounter, out.HasCounter = in.FrameCounter, in.HasCounter
	out.Left.Status, out.Right.Status = in.Left.Status, in.Right.Status
	if out != in {
		t.Fatalf("roundtrip mismatch\n in %+v\nout %+v", in, out)
	}
}

func TestMarshalZeroAlloc(t *testing.T) {
	s := sample()
	var p [PacketSize]byte
	if a := testing.AllocsPerRun(1000, func() { Marshal(&p, &s) }); a != 0 {
		t.Fatalf("Marshal allocates %.1f", a)
	}
}

func TestUDPDelivery(t *testing.T) {
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	u, err := NewUDP(l.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	s := sample()
	if err := u.Write(&s); err != nil {
		t.Fatal(err)
	}
	if a := testing.AllocsPerRun(200, func() { _ = u.Write(&s) }); a != 0 {
		t.Errorf("UDP.Write allocates %.1f", a)
	}
	buf := make([]byte, 2048)
	_ = l.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := l.ReadFromUDP(buf)
	if err != nil || n != PacketSize {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var got gaze.Sample
	if err := Unmarshal(buf[:n], &got); err != nil || got.Seq != 99 || got.Gaze != s.Gaze {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// Offset dichiarati nel commento del formato (e nella stringa struct di Python).
func TestHeadRotationImageFlags(t *testing.T) {
	in := sample()
	in.Head.RotationValid = [3]bool{true, true, true}
	in.Head.RotationImage = [3]bool{true, true, false}
	var p [PacketSize]byte
	Marshal(&p, &in)
	f2 := binary.LittleEndian.Uint32(p[120:])
	if f2&(Flag2HeadPitchImage|Flag2HeadYawImage|Flag2HeadRollImage) != Flag2HeadPitchImage|Flag2HeadYawImage ||
		Flag2HeadPitchImage != 1<<6 || Flag2HeadRollImage != 1<<8 {
		t.Fatalf("flag2 %09b", f2)
	}
	var out gaze.Sample
	if err := Unmarshal(p[:], &out); err != nil || out.Head.RotationImage != in.Head.RotationImage {
		t.Fatalf("roundtrip %+v %v", out.Head, err)
	}
}

func TestPacketV2Layout(t *testing.T) {
	in := sample()
	var p [PacketSize]byte
	Marshal(&p, &in)
	le := binary.LittleEndian
	f32 := func(off int) float32 { return math.Float32frombits(le.Uint32(p[off:])) }
	if p[4] != 2 || string(p[0:4]) != "T4CG" {
		t.Fatalf("header % x", p[:8])
	}
	if f := le.Uint32(p[120:]); f != Flag2LeftOriginDisplay|Flag2LeftTrackBox|Flag2LeftOpenness {
		t.Errorf("flag2 %06b", f)
	}
	if p[124] != byte(gaze.PresencePresent) || p[125]|p[126]|p[127] != 0 {
		t.Errorf("presence % x", p[124:128])
	}
	if int64(le.Uint64(p[128:])) != in.PresenceUs {
		t.Error("presence_us")
	}
	checks := map[int]float32{
		48: in.Head.PositionMM[0], 136: in.Left.OriginDisplayMM[0], 148: in.Right.OriginDisplayMM[0],
		160: in.Left.TrackBox[0], 172: in.Right.TrackBox[0], 184: in.Left.OpennessMM, 188: in.Right.OpennessMM,
	}
	for off, want := range checks {
		if got := f32(off); got != want {
			t.Errorf("offset %d: %v, atteso %v", off, got, want)
		}
	}
}

// Un ricevitore aggiornato legge anche i pacchetti v1 (120 byte).
func TestUnmarshalV1(t *testing.T) {
	in := sample()
	var p [PacketSize]byte
	Marshal(&p, &in)
	p[4] = 1
	var out gaze.Sample
	if err := Unmarshal(p[:PacketSizeV1], &out); err != nil {
		t.Fatal(err)
	}
	if out.Gaze != in.Gaze || out.Left.OriginMM != in.Left.OriginMM || out.Right.Gaze2D != in.Right.Gaze2D {
		t.Fatalf("campi v1: %+v", out)
	}
	if out.Presence != gaze.PresenceUnknown || out.Left.OriginDisplayValid || out.Left.OpennessMM != 0 {
		t.Fatalf("un pacchetto v1 non ha campi v2: %+v", out.Left)
	}
	p[4] = 2
	if err := Unmarshal(p[:PacketSizeV1], &out); err == nil {
		t.Fatal("un pacchetto v2 troncato a 120 byte va rifiutato")
	}
	p[4] = 3
	if err := Unmarshal(p[:], &out); err == nil {
		t.Fatal("versione 3 sconosciuta va rifiutata")
	}
}

func TestLSLChannels(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range lslChannels {
		if c == "" || seen[c] {
			t.Fatalf("canale vuoto o duplicato %q", c)
		}
		seen[c] = true
	}
	s := sample()
	var v [lslChannelCount]float32
	lslFill(&v, &s)
	idx := func(name string) int {
		for i, c := range lslChannels {
			if c == name {
				return i
			}
		}
		t.Fatalf("canale %q assente", name)
		return -1
	}
	if v[idx("gaze_x")] != s.Gaze[0] || v[idx("head_z_mm")] != s.Head.PositionMM[2] || v[idx("head_valid")] != 1 {
		t.Error("canali v1")
	}
	if v[idx("presence")] != 2 || v[idx("origin_left_y_mm")] != 151 || v[idx("trackbox_left_z")] != s.Left.TrackBox[2] ||
		v[idx("openness_left_mm")] != 10.25 {
		t.Errorf("canali nuovi: %v", v)
	}
	// Destro: origine display, track box e apertura non valide → NaN.
	for _, c := range []string{"origin_right_x_mm", "trackbox_right_y", "openness_right_mm"} {
		if !math.IsNaN(float64(v[idx(c)])) {
			t.Errorf("%s = %v, atteso NaN", c, v[idx(c)])
		}
	}
	if v[idx("head_yaw_rad")] != s.Head.RotationRad[1] || !math.IsNaN(float64(v[idx("head_pitch_rad")])) {
		t.Errorf("angoli della testa: %v", v[idx("head_pitch_rad"):idx("head_valid")+1])
	}
	// Yaw dal viso con un occhio solo: angoli validi, posizione no.
	s.Head.PositionValid, s.Head.RotationValid = false, [3]bool{true, true, true}
	lslFill(&v, &s)
	if v[idx("head_valid")] != 0 || v[idx("head_yaw_rad")] != s.Head.RotationRad[1] || math.IsNaN(float64(v[idx("head_pitch_rad")])) {
		t.Errorf("yaw dal viso: %v", v[idx("head_pitch_rad"):idx("head_valid")+1])
	}
	s.Head.RotationValid = [3]bool{}
	lslFill(&v, &s)
	if !math.IsNaN(float64(v[idx("head_yaw_rad")])) || !math.IsNaN(float64(v[idx("head_roll_rad")])) {
		t.Errorf("yaw/roll non validi: %v", v[idx("head_pitch_rad"):idx("head_valid")+1])
	}
	if a := testing.AllocsPerRun(1000, func() { lslFill(&v, &s) }); a != 0 {
		t.Fatalf("lslFill alloca %.1f", a)
	}
}
