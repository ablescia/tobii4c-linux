// Package sink contiene le destinazioni dei campioni: UDP locale, Lab
// Streaming Layer (build tag "lsl") e stampa diagnostica.
package sink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

// Formato del datagramma UDP, versione 2: 192 byte, TUTTO little-endian.
// I primi 120 byte hanno gli stessi offset della versione 1; i campi nuovi
// sono accodati.
//
//	off  size  campo
//	  0    4   magic "T4CG"
//	  4    1   versione (2)
//	  5    1   sorgente head pose (0 nessuna, 1 derivata, 2 dispositivo)
//	  6    2   flag di validità (vedi Flag*)
//	  8    8   seq          u64   contatore host, monotono
//	 16    8   host_ns      i64   ricezione USB, CLOCK_REALTIME ns
//	 24    8   device_us    i64   orologio del tracker, µs
//	 32    8   gaze xy      2×f32 combinato filtrato, normalizzato [0,1] sul display
//	 40    8   gaze_raw xy  2×f32 combinato non filtrato
//	 48   12   head pos     3×f32 mm dal centro dello schermo (v1: sistema del tracker)
//	 60   12   head rot     3×f32 rad (pitch, yaw, roll) attorno agli assi dello schermo
//	 72   12   left origin  3×f32 mm, sistema del tracker
//	 84   12   right origin 3×f32 mm, sistema del tracker
//	 96    4   left pupil   f32 mm
//	100    4   right pupil  f32 mm
//	104    8   left gaze2d  2×f32
//	112    8   right gaze2d 2×f32
//	--- v2 ---
//	120    4   flag2        u32   validità dei campi v2 (vedi Flag2*)
//	124    1   presence     u8    0 sconosciuta, 1 assente, 2 presente
//	125    3   (zero)
//	128    8   presence_us  i64   orologio del tracker all'ultimo messaggio di presenza (0 = nessuno)
//	136   12   left origin display  3×f32 mm dal centro dello schermo
//	148   12   right origin display 3×f32 mm dal centro dello schermo
//	160   12   left trackbox        3×f32 normalizzato [0,1]
//	172   12   right trackbox       3×f32 normalizzato [0,1]
//	184    4   left openness        f32 mm
//	188    4   right openness       f32 mm
//
// Python: struct.unpack('<4sBBHQqq2f2f3f3f3f3fff2f2fIB3xq3f3f3f3fff', data)
const (
	PacketSize    = 192
	PacketVersion = 2
	PacketSizeV1  = 120 // Unmarshal accetta anche i pacchetti v1
)

// Flag di validità (campo a offset 6).
const (
	FlagGaze uint16 = 1 << iota
	FlagGazeRaw
	FlagLeftTracked
	FlagRightTracked
	FlagLeftOrigin
	FlagRightOrigin
	FlagHeadPosition
	FlagHeadPitch
	FlagHeadYaw
	FlagHeadRoll
	FlagLeftPupil
	FlagRightPupil
	FlagLeftGaze2D
	FlagRightGaze2D
)

// Flag di validità dei campi v2 (campo a offset 120).
const (
	Flag2LeftOriginDisplay uint32 = 1 << iota
	Flag2RightOriginDisplay
	Flag2LeftTrackBox
	Flag2RightTrackBox
	Flag2LeftOpenness
	Flag2RightOpenness
)

// Marshal serializza s in p senza allocare.
func Marshal(p *[PacketSize]byte, s *gaze.Sample) {
	le := binary.LittleEndian
	copy(p[0:4], "T4CG")
	p[4] = PacketVersion
	p[5] = byte(s.Head.Source)
	var f uint16
	set := func(b bool, flag uint16) {
		if b {
			f |= flag
		}
	}
	set(s.GazeValid, FlagGaze)
	set(s.GazeRawValid, FlagGazeRaw)
	set(s.Left.Tracked, FlagLeftTracked)
	set(s.Right.Tracked, FlagRightTracked)
	set(s.Left.OriginValid, FlagLeftOrigin)
	set(s.Right.OriginValid, FlagRightOrigin)
	set(s.Head.PositionValid, FlagHeadPosition)
	set(s.Head.RotationValid[0], FlagHeadPitch)
	set(s.Head.RotationValid[1], FlagHeadYaw)
	set(s.Head.RotationValid[2], FlagHeadRoll)
	set(s.Left.PupilValid, FlagLeftPupil)
	set(s.Right.PupilValid, FlagRightPupil)
	set(s.Left.Gaze2DValid, FlagLeftGaze2D)
	set(s.Right.Gaze2DValid, FlagRightGaze2D)
	var f2 uint32
	set2 := func(b bool, flag uint32) {
		if b {
			f2 |= flag
		}
	}
	set2(s.Left.OriginDisplayValid, Flag2LeftOriginDisplay)
	set2(s.Right.OriginDisplayValid, Flag2RightOriginDisplay)
	set2(s.Left.TrackBoxValid, Flag2LeftTrackBox)
	set2(s.Right.TrackBoxValid, Flag2RightTrackBox)
	set2(s.Left.OpennessValid, Flag2LeftOpenness)
	set2(s.Right.OpennessValid, Flag2RightOpenness)
	le.PutUint16(p[6:], f)
	le.PutUint64(p[8:], s.Seq)
	le.PutUint64(p[16:], uint64(s.HostTimeNs))
	le.PutUint64(p[24:], uint64(s.DeviceTimeUs))
	off := 32
	put := func(vs ...float32) {
		for _, v := range vs {
			le.PutUint32(p[off:], math.Float32bits(v))
			off += 4
		}
	}
	put(s.Gaze[0], s.Gaze[1], s.GazeRaw[0], s.GazeRaw[1])
	put(s.Head.PositionMM[:]...)
	put(s.Head.RotationRad[:]...)
	put(s.Left.OriginMM[:]...)
	put(s.Right.OriginMM[:]...)
	put(s.Left.PupilMM, s.Right.PupilMM)
	put(s.Left.Gaze2D[:]...)
	put(s.Right.Gaze2D[:]...)

	le.PutUint32(p[120:], f2)
	p[124], p[125], p[126], p[127] = byte(s.Presence), 0, 0, 0
	le.PutUint64(p[128:], uint64(s.PresenceUs))
	off = 136
	put(s.Left.OriginDisplayMM[:]...)
	put(s.Right.OriginDisplayMM[:]...)
	put(s.Left.TrackBox[:]...)
	put(s.Right.TrackBox[:]...)
	put(s.Left.OpennessMM, s.Right.OpennessMM)
}

// Unmarshal è l'inverso di Marshal (per ricevitori e test). Accetta anche i
// pacchetti v1, che non hanno i campi della v2.
func Unmarshal(p []byte, s *gaze.Sample) error {
	if len(p) < PacketSizeV1 || string(p[0:4]) != "T4CG" {
		return errors.New("not a T4CG packet")
	}
	switch {
	case p[4] == 1:
	case p[4] == PacketVersion && len(p) >= PacketSize:
	case p[4] == PacketVersion:
		return fmt.Errorf("truncated T4CG v2 packet: %d bytes", len(p))
	default:
		return fmt.Errorf("unsupported packet version %d", p[4])
	}
	le := binary.LittleEndian
	f := le.Uint16(p[6:])
	has := func(flag uint16) bool { return f&flag != 0 }
	*s = gaze.Sample{
		Seq:          le.Uint64(p[8:]),
		HostTimeNs:   int64(le.Uint64(p[16:])),
		DeviceTimeUs: int64(le.Uint64(p[24:])),
		GazeValid:    has(FlagGaze), GazeRawValid: has(FlagGazeRaw),
	}
	s.Head.Source = gaze.HeadSource(p[5])
	s.Left.Tracked, s.Right.Tracked = has(FlagLeftTracked), has(FlagRightTracked)
	s.Left.OriginValid, s.Right.OriginValid = has(FlagLeftOrigin), has(FlagRightOrigin)
	s.Head.PositionValid = has(FlagHeadPosition)
	s.Head.RotationValid = [3]bool{has(FlagHeadPitch), has(FlagHeadYaw), has(FlagHeadRoll)}
	s.Left.PupilValid, s.Right.PupilValid = has(FlagLeftPupil), has(FlagRightPupil)
	s.Left.Gaze2DValid, s.Right.Gaze2DValid = has(FlagLeftGaze2D), has(FlagRightGaze2D)
	off := 32
	get := func(dst ...*float32) {
		for _, d := range dst {
			*d = math.Float32frombits(le.Uint32(p[off:]))
			off += 4
		}
	}
	get3 := func(v *[3]float32) { get(&v[0], &v[1], &v[2]) }
	get(&s.Gaze[0], &s.Gaze[1], &s.GazeRaw[0], &s.GazeRaw[1])
	get3(&s.Head.PositionMM)
	get3(&s.Head.RotationRad)
	get3(&s.Left.OriginMM)
	get3(&s.Right.OriginMM)
	get(&s.Left.PupilMM, &s.Right.PupilMM)
	get(&s.Left.Gaze2D[0], &s.Left.Gaze2D[1], &s.Right.Gaze2D[0], &s.Right.Gaze2D[1])
	if p[4] == 1 {
		return nil
	}

	f2 := le.Uint32(p[120:])
	has2 := func(flag uint32) bool { return f2&flag != 0 }
	s.Left.OriginDisplayValid, s.Right.OriginDisplayValid = has2(Flag2LeftOriginDisplay), has2(Flag2RightOriginDisplay)
	s.Left.TrackBoxValid, s.Right.TrackBoxValid = has2(Flag2LeftTrackBox), has2(Flag2RightTrackBox)
	s.Left.OpennessValid, s.Right.OpennessValid = has2(Flag2LeftOpenness), has2(Flag2RightOpenness)
	s.Presence = gaze.Presence(p[124])
	s.PresenceUs = int64(le.Uint64(p[128:]))
	off = 136
	get3(&s.Left.OriginDisplayMM)
	get3(&s.Right.OriginDisplayMM)
	get3(&s.Left.TrackBox)
	get3(&s.Right.TrackBox)
	get(&s.Left.OpennessMM, &s.Right.OpennessMM)
	return nil
}

// UDP invia un datagramma per campione a un indirizzo (tipicamente locale).
type UDP struct {
	conn *net.UDPConn
	buf  [PacketSize]byte
}

// NewUDP apre il socket verso addr (es. "127.0.0.1:5005").
func NewUDP(addr string) (*UDP, error) {
	ra, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.DialUDP("udp", nil, ra)
	if err != nil {
		return nil, err
	}
	_ = c.SetWriteBuffer(1 << 16)
	return &UDP{conn: c}, nil
}

// Write serializza e invia. Senza ricevitore in ascolto il kernel risponde
// ICMP port-unreachable: l'errore ECONNREFUSED che ne deriva è ignorato.
func (u *UDP) Write(s *gaze.Sample) error {
	Marshal(&u.buf, s)
	_, err := u.conn.Write(u.buf[:])
	if err != nil && isConnRefused(err) {
		return nil
	}
	return err
}

// Close chiude il socket.
func (u *UDP) Close() error { return u.conn.Close() }
