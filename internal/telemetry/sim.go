package telemetry

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/usbdev"
)

// Simulazione: un 4C finto con i valori di identità, configurazione e
// colonne di un 4C vero (firmware 4.7.0-d374125, letti il 2026-11-07) e uno
// sguardo che si muove. Serve a provare la finestra senza il dispositivo.

// Risposte del 4C alle interrogazioni di montaggio (0x83e) e track box (0x578).
const (
	sim4CMounting = "0000020000000400000002030000000400b80000030000000400140000050000000400031f41040000000800000000000000000400000008fffff0000000000004000000080000300000000000050000000400031f41040000000800000000000000000400000008000024000000000004000000080000200000000000"
	sim4CTrackBox = "0000050000000400031f41040000000800025800000000000400000008000190000000000004000000080007080000000000050000000400031f410400000008fffda800000000000400000008000190000000000004000000080007080000000000050000000400031f410400000008fffda800000000000400000008fffe70000000000004000000080007080000000000050000000400031f41040000000800025800000000000400000008fffe70000000000004000000080007080000000000050000000400031f41040000000800044c0000000000040000000800032000000000000400000008000e100000000000050000000400031f410400000008fffbb40000000000040000000800032000000000000400000008000e100000000000050000000400031f410400000008fffbb400000000000400000008fffce000000000000400000008000e100000000000050000000400031f41040000000800044c00000000000400000008fffce000000000000400000008000e100000000000"
)

// sim4CColumns sono le colonne dello stream gaze dichiarate dal 4C, nel suo
// ordine (`tobii4c info`).
var sim4CColumns = []ttp.Record{
	{ID: 0x01, Name: "Timestamp"}, {ID: 0x11}, {ID: 0x14},
	{ID: 0x1b, Name: "Relative Combined Gaze Validity"}, {ID: 0x1c, Name: "Relative Combined Gaze Point"},
	{ID: 0x16, Name: "Left Gaze Origin Tracker Validity"}, {ID: 0x02, Name: "Left Gaze Origin Tracker"},
	{ID: 0x21, Name: "Left Gaze Origin Display Validity"}, {ID: 0x22, Name: "Left Gaze Origin Display"},
	{ID: 0x03, Name: "Left Eye 3D Relative Position"}, {ID: 0x1d, Name: "Left Eye 3D Relative Position Validity"},
	{ID: 0x04, Name: "Left Eye 3D Gaze Point"}, {ID: 0x05, Name: "Left Eye 2D Gaze Point"},
	{ID: 0x06, Name: "Left Eye Pupil Diameter"}, {ID: 0x07, Name: "Left Eye Validity"},
	{ID: 0x17, Name: "Left EyeBall Center position"}, {ID: 0x19, Name: "Left eye 2D position relative to sensor area"},
	{ID: 0x15, Name: "Right Gaze Origin Tracker Validity"}, {ID: 0x08, Name: "Right Gaze Origin Tracker"},
	{ID: 0x23, Name: "Right Gaze Origin Display Validity"}, {ID: 0x24, Name: "Right Gaze Origin Display"},
	{ID: 0x09, Name: "Right Eye 3D Relative Position"}, {ID: 0x1e, Name: "Right Eye 3D Relative Position Validity"},
	{ID: 0x0a, Name: "Right Eye 3D Gaze Point"}, {ID: 0x0b, Name: "Right Eye 2D Gaze Point"},
	{ID: 0x0c, Name: "Right Eye Pupil Diameter"}, {ID: 0x0d, Name: "Right Eye Validity"},
	{ID: 0x18, Name: "Right EyeBall Center position"}, {ID: 0x1a, Name: "Right eye 2D position relative to sensor area"},
	{ID: 0x1f, Name: "Remote Gaze Point Responsive Combined Validity"}, {ID: 0x20, Name: "Remote Gaze Point Responsive Combined"},
	{ID: 0x25, Name: "Left User 3D Position Guide"}, {ID: 0x26, Name: "Left User 3D Position Guide Validity"},
	{ID: 0x27, Name: "Right User 3D Position Guide"}, {ID: 0x28, Name: "Right User 3D Position Guide Validity"},
	{ID: 0x29, Name: "Left Absolute Eye Openness"}, {ID: 0x2a, Name: "Left Absolute Eye Openness Validity"},
	{ID: 0x2b, Name: "Right Absolute Eye Openness"}, {ID: 0x2c, Name: "Right Absolute Eye Openness Validity"},
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// simSession è la sessione del 4C simulato.
func simSession() SessionInfo {
	m, _ := ttp.ParseMounting(mustHex(sim4CMounting))
	tb, _ := ttp.ParseTrackBox(mustHex(sim4CTrackBox))
	d := ttp.DisplayArea{TopLeft: [3]float64{-339.458, 381.805, 130.920}, TopRight: [3]float64{350.965, 381.805, 130.920},
		BottomLeft: [3]float64{-339.458, 16.863, -1.908}}
	cat := []ttp.Record{{ID: 0x500, Name: "gaze"}, {ID: 0x501, Name: "image"}, {ID: 0x504, Name: "presence"},
		{ID: 0x508, Name: "image_collection"}, {ID: 0x1770, Name: "algodbg"}, {ID: 0x1772, Name: "log"}}
	return SessionInfo{
		Connected: true, Since: time.Now(), Sessions: 1,
		USB: usbdev.Info{VendorID: 0x2104, ProductID: 0x0127, Bus: 5, Address: 6, Manufacturer: "Tobii Technology AB",
			Product: "EyeChip", Serial: "IS404-100108121275", Speed: "high"},
		Strings:   []string{"IS404-100108121275", "IS4_Large_Peripheral", "IS4", "4.7.0-d374125"},
		Protocol:  []uint32{0x10008},
		Realm:     []uint32{0},
		Catalogue: cat,
		Columns: map[uint32][]ttp.Record{
			0x500: sim4CColumns,
			0x501: {{ID: 1, Name: "Timestamp"}, {ID: 2, Name: "Bits per pixel"}, {ID: 3, Name: "Width"}, {ID: 4, Name: "Height"}, {ID: 5, Name: "Image"}},
			0x504: {{ID: 1, Name: "Timestamp"}, {ID: 2, Name: "Presence enum value"}},
			0x508: {{ID: 1, Name: "Timestamp"}, {ID: 2, Name: "Bits per pixel"}, {ID: 3, Name: "Frame format"}, {ID: 4, Name: "Frame number"},
				{ID: 5, Name: "Color information"}, {ID: 6, Name: "Encryption type"}, {ID: 7, Name: "TDI System type"},
				{ID: 8, Name: "Region group Conf list"}, {ID: 9, Name: "Little endian"}, {ID: 0xa, Name: "X skip"}, {ID: 0xb, Name: "Y skip"},
				{ID: 0xc, Name: "X bin"}, {ID: 0xd, Name: "Y bin"}, {ID: 0xe, Name: "LED Conf list"}, {ID: 0xf, Name: "Image"}},
			0x1770: {{ID: 1, Name: "Debug data"}},
			0x1772: {{ID: 1, Name: "Log entries"}},
		},
		Display: &d, Mounting: &m, TrackBox: &tb,
		Properties: []ttp.Property{{Key: 0, Value: "Peripheral"}, {Key: 2, Value: "HELLO_NIR"}, {Key: 3, Value: "IS4LTOBIIPERIPHERAL"},
			{Key: 5, Value: "false"}, {Key: 6, Value: "false"}, {Key: 7, Value: "false"}, {Key: 0xc, Value: "3"}},
		Status: []ttp.Property{{Key: 0, Value: "false"}, {Key: 1, Value: "false"}, {Key: 2, Value: "false"}, {Key: 3, Value: "false"},
			{Key: 4, Value: ""}, {Key: 5, Value: "ok"}, {Key: 6, Value: "ok"}, {Key: 7, Value: "2830289978"}, {Key: 8, Value: "false"}},
		OutputRate: []uint32{90, 90}, EnabledEye: []uint32{3},
		Subscribed: []uint32{0x500, 0x504, 0x501, 0x1772}, Refused: map[uint32]string{}, Probes: map[string]string{},
	}
}

// simSample è un campione plausibile all'istante t (secondi): lo sguardo
// percorre una curva di Lissajous, la pupilla oscilla, ogni 4 s un
// ammiccamento.
func simSample(seq uint64, hostNs int64, t float64) gaze.Sample {
	s := gaze.Sample{Seq: seq, HostTimeNs: hostNs, DeviceTimeUs: 74709605964 + int64(t*1e6), FrameCounter: 78676 + uint32(seq),
		HasCounter: true, Presence: gaze.PresencePresent}
	gx, gy := float32(0.5+0.3*math.Sin(t*0.9)), float32(0.45+0.25*math.Sin(t*1.7))
	s.Gaze, s.GazeValid = [2]float32{gx, gy}, true
	s.GazeRaw, s.GazeRawValid = [2]float32{gx + 0.01*float32(math.Sin(t*23)), gy - 0.008*float32(math.Cos(t*19))}, true
	blink := math.Mod(t, 4) < 0.15
	head := float32(25 * math.Sin(t*0.3))
	for k, e := range [2]*gaze.Eye{&s.Left, &s.Right} {
		dx := float32(-31 + 62*k)
		e.Tracked, e.OriginValid, e.OriginDisplayValid, e.Gaze2DValid, e.TrackBoxValid = true, true, true, true, true
		e.OriginMM = [3]float32{dx + 12 + head, 95, 610}
		e.OriginDisplayMM = [3]float32{dx + 5 + head, -112, 640}
		e.Gaze2D = [2]float32{gx + float32(k)*0.02 - 0.01, gy + 0.01}
		e.TrackBox = [3]float32{0.52 - 0.1*float32(k) - head/600, 0.4, 0.35}
		e.PupilMM, e.PupilValid = float32(3.6+0.3*math.Sin(t*0.4)+0.1*float64(k)), true
		e.OpennessMM, e.OpennessValid = float32(10.5+0.6*math.Sin(t*0.7)), true
		if blink {
			e.OpennessMM, e.PupilMM, e.PupilValid = 1.2, -1, false
		}
	}
	gaze.DeriveHead(&s)
	return s
}

// simRow è la riga grezza del firmware coerente con s, con tutte le 39
// colonne del 4C.
func simRow(s *gaze.Sample) ttp.Row {
	var r ttp.Row
	add := func(k uint32, v ttp.Value) {
		r.Cols[r.N] = ttp.Column{Key: k, Value: v}
		r.N++
	}
	u := func(v uint32) ttp.Value { return ttp.Value{Kind: ttp.KindU32, U: v} }
	b := func(ok bool) ttp.Value {
		if ok {
			return u(1)
		}
		return u(0)
	}
	p2 := func(p [2]float32) ttp.Value {
		return ttp.Value{Kind: ttp.KindPoint, N: 2, F: [3]float64{float64(p[0]), float64(p[1])}}
	}
	p3 := func(p [3]float32) ttp.Value {
		return ttp.Value{Kind: ttp.KindPoint, N: 3, F: [3]float64{float64(p[0]), float64(p[1]), float64(p[2])}}
	}
	f := func(v float32) ttp.Value { return ttp.Value{Kind: ttp.KindFixed16, F: [3]float64{float64(v)}} }
	add(0x01, ttp.Value{Kind: ttp.KindS64, I: s.DeviceTimeUs})
	add(0x11, u(uint32(s.Seq)))
	add(0x14, u(s.FrameCounter))
	add(0x1b, b(s.GazeValid))
	add(0x1c, p2(s.Gaze))
	for k, e := range [2]*gaze.Eye{&s.Left, &s.Right} {
		add([]uint32{0x16, 0x15}[k], b(e.OriginValid))
		add([]uint32{0x02, 0x08}[k], p3(e.OriginMM))
		add([]uint32{0x21, 0x23}[k], b(e.OriginDisplayValid))
		add([]uint32{0x22, 0x24}[k], p3(e.OriginDisplayMM))
		add([]uint32{0x03, 0x09}[k], p3(e.TrackBox))
		add([]uint32{0x1d, 0x1e}[k], b(e.TrackBoxValid))
		add([]uint32{0x04, 0x0a}[k], p3([3]float32{e.OriginDisplayMM[0] + 40, 20, 0}))
		add([]uint32{0x05, 0x0b}[k], p2(e.Gaze2D))
		add([]uint32{0x06, 0x0c}[k], f(e.PupilMM))
		add([]uint32{0x07, 0x0d}[k], u(e.Status))
		add([]uint32{0x17, 0x18}[k], p3([3]float32{e.OriginMM[0] - 1, e.OriginMM[1] + 2, e.OriginMM[2] + 11}))
		add([]uint32{0x19, 0x1a}[k], p2([2]float32{-1, -1}))
	}
	add(0x1f, b(s.GazeRawValid))
	add(0x20, p2(s.GazeRaw))
	for k, e := range [2]*gaze.Eye{&s.Left, &s.Right} {
		add([]uint32{0x25, 0x27}[k], p3(e.TrackBox))
		add([]uint32{0x26, 0x28}[k], u(1))
	}
	for k, e := range [2]*gaze.Eye{&s.Left, &s.Right} {
		add([]uint32{0x29, 0x2b}[k], f(e.OpennessMM))
		add([]uint32{0x2a, 0x2c}[k], b(e.OpennessValid))
	}
	r.Declared = r.N
	return r
}

// Simulate alimenta il collettore come farebbe il tracker, finché ctx non
// termina: campioni a 90 Hz con la loro riga grezza (passata all'hook come
// payload XDS), presenza, righe di log e, se img non è nil, la stessa
// immagine a 30 Hz passata allo stimatore del viso (est, se non nil).
func Simulate(ctx context.Context, c *Collector, est *headpose.Estimator, img *irimg.Frame) {
	info := simSession()
	c.SetSession(info)
	if est != nil {
		est.SetDevice(info.USB.Serial, info.Display)
	}
	pres := func(p gaze.Presence, us int64) {
		var r ttp.Row
		r.Cols[0] = ttp.Column{Key: gaze.PresenceKeyTimestamp, Value: ttp.Value{Kind: ttp.KindS64, I: us}}
		r.Cols[1] = ttp.Column{Key: gaze.PresenceKeyValue, Value: ttp.Value{Kind: ttp.KindU32, U: uint32(p)}}
		r.N, r.Declared = 2, 2
		c.Hook(ttp.StreamPresence, time.Now().UnixNano(), ttp.AppendRow(nil, &r))
	}
	t0 := time.Now()
	pres(gaze.PresencePresent, 74709605964)
	tk := time.NewTicker(time.Second / 90)
	defer tk.Stop()
	var buf, ibuf []byte
	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			seq++
			t := now.Sub(t0).Seconds()
			s := simSample(seq, now.UnixNano(), t)
			row := simRow(&s)
			buf = ttp.AppendRow(buf[:0], &row)
			c.Hook(ttp.StreamGaze, s.HostTimeNs, buf)
			if est != nil {
				est.Observe(&s)
			}
			_ = c.Write(&s)
			if img != nil && seq%3 == 0 {
				img.DeviceUs = s.DeviceTimeUs - 25_000
				ibuf = irimg.AppendPayload(ibuf[:0], img)
				c.Hook(ttp.StreamImage, s.HostTimeNs, ibuf)
				if est != nil {
					est.OfferImage(ibuf, s.HostTimeNs)
				}
			}
			if seq%450 == 0 { // ogni 5 s una riga di log del firmware
				c.AddLog(LogLine{Time: now, Firmware: true, Level: 'I',
					Text: fmt.Sprintf("[%13.6f] (I) TDI thread: Changed run mode to %d", float64(s.DeviceTimeUs)/1e6, 21+seq/450%2)})
			}
		}
	}
}
