//go:build linux

package main

import (
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
)

// I numeri degli ioctl derivano dalle dimensioni delle strutture: devono
// coincidere con quelli di <linux/videodev2.h> su Linux a 64 bit.
func TestIoctlNumbers(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("valori di riferimento a 64 bit")
	}
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"VIDIOC_QUERYCAP", vidiocQueryCap, 0x80685600},
		{"VIDIOC_ENUM_FMT", vidiocEnumFmt, 0xc0405602},
		{"VIDIOC_G_FMT", vidiocGFmt, 0xc0d05604},
		{"VIDIOC_S_FMT", vidiocSFmt, 0xc0d05605},
		{"VIDIOC_REQBUFS", vidiocReqBufs, 0xc0145608},
		{"VIDIOC_QUERYBUF", vidiocQueryBuf, 0xc0585609},
		{"VIDIOC_QBUF", vidiocQBuf, 0xc058560f},
		{"VIDIOC_DQBUF", vidiocDQBuf, 0xc0585611},
		{"VIDIOC_STREAMON", vidiocStreamOn, 0x40045612},
		{"VIDIOC_STREAMOFF", vidiocStreamOff, 0x40045613},
		{"VIDIOC_G_PARM", vidiocGParm, 0xc0cc5615},
		{"VIDIOC_S_PARM", vidiocSParm, 0xc0cc5616},
		{"VIDIOC_G_CTRL", vidiocGCtrl, 0xc008561b},
		{"VIDIOC_QUERYCTRL", vidiocQueryCtrl, 0xc0445624},
		{"VIDIOC_QUERYMENU", vidiocQueryMenu, 0xc02c5625},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, atteso %#x", c.name, c.got, c.want)
		}
	}
	var b v4l2Buffer
	if off := unsafe.Offsetof(b.M); off != 64 {
		t.Errorf("v4l2_buffer.m all'offset %d, atteso 64", off)
	}
}

func TestToGrayYUYV(t *testing.T) {
	const w, h, bpl = 4, 2, 10 // 2 byte di riempimento per riga
	data := []byte{
		10, 128, 20, 128, 30, 128, 40, 128, 0, 0,
		50, 128, 60, 128, 70, 128, 80, 128, 0, 0,
	}
	dst := make([]byte, w*h)
	if err := toGray(pixYUYV, data, w, h, bpl, dst); err != nil {
		t.Fatal(err)
	}
	want := []byte{10, 20, 30, 40, 50, 60, 70, 80}
	if string(dst) != string(want) {
		t.Fatalf("luminanza %v, attesa %v", dst, want)
	}
	if err := toGray(pixYUYV, data[:12], w, h, bpl, dst); err == nil {
		t.Fatal("frame troncato accettato")
	}
}

func TestBlinker(t *testing.T) {
	var b blinker
	t0 := time.Unix(1000, 0)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * 33 * time.Millisecond) }
	i := 0
	feed := func(ear float64, n int) {
		for k := 0; k < n; k++ {
			b.update(ear, at(i), true)
			i++
		}
	}
	feed(0.30, 60)
	feed(0.12, 4) // ammiccamento di ~130 ms
	feed(0.30, 30)
	feed(0.10, 5)
	feed(0.30, 30)
	if b.total != 2 {
		t.Fatalf("%d ammiccamenti, attesi 2", b.total)
	}
	// sguardo in basso: l'EAR resta più basso a lungo, il riferimento si
	// adatta e gli occhi tornano "aperti" senza contare ammiccamenti
	feed(0.19, 300)
	if b.closed {
		t.Fatalf("occhi ancora chiusi con EAR stabile (riferimento %.3f)", b.base)
	}
	if b.total != 2 {
		t.Fatalf("%d ammiccamenti dopo lo sguardo in basso, attesi 2", b.total)
	}
}

func TestNiceStep(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{{0.03, 0.05}, {2.5, 5}, {7, 10}, {10, 10}, {86.8, 100}} {
		if got := niceStep(c.in); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("niceStep(%v) = %v, atteso %v", c.in, got, c.want)
		}
	}
}

// testFrame è il viso di TOBII4C_FACE_IMAGE (PGM) se c'è, altrimenti un
// gradiente senza viso.
func testFrame(t *testing.T) *frame {
	if path := os.Getenv("TOBII4C_FACE_IMAGE"); path != "" {
		f, err := irimg.ReadPGMFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return &frame{Frame: *f, seq: 1, captured: time.Now()}
	}
	const w, h = 640, 480
	f := &frame{Frame: irimg.Frame{W: w, H: h, Pix: make([]byte, w*h)}, seq: 1, captured: time.Now()}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			f.Pix[y*w+x] = byte((x + y) * 255 / (w + h))
		}
	}
	return f
}

// TestRender elabora un frame con il modello vero e disegna l'interfaccia a
// più dimensioni, senza X e senza webcam. Con WEBCAM_FACE_PNG=<cartella>
// salva le schermate per un controllo a vista.
func TestRender(t *testing.T) {
	det, err := headpose.NewDetector("", 2)
	if err != nil {
		t.Fatal(err)
	}
	f := testFrame(t)
	eng := &Engine{det: det, Focal: 554, FOV: 60, Workers: 2, MACs: det.MACs(), Model: "incorporato", ModelOK: det.Verified,
		Info: CamInfo{Path: "/dev/video0", Card: "Webcam di prova", Driver: "uvcvideo", Version: "7.2.15", Bus: "usb-test",
			W: f.W, H: f.H, PixFmt: pixYUYV, BytesPerLine: 2 * f.W, FPS: 30, Formats: []string{"MJPG", "YUYV"},
			Buffers: 4, BufferBytes: 2 * f.W * f.H, USB: USBInfo{ID: "1bcf:2281", Product: "prova", Speed: "480"}},
		ctrls: []Control{
			{ID: 0x00980900, Name: "Brightness", Type: ctrlTypeInteger, Max: 255, Default: 128, Value: 137, OK: true},
			{ID: 0x009a0901, Name: "Auto Exposure", Type: ctrlTypeMenu, Max: 3, Default: 3, Value: 3, OK: true,
				Menu: map[int32]string{1: "Manual Mode", 3: "Aperture Priority Mode"}},
			{ID: 0x009a0902, Name: "Exposure Time, Absolute", Type: ctrlTypeInteger, Max: 2047, Default: 166, Value: 166,
				OK: true, Flags: ctrlFlagInactive},
		}}
	for i := 0; i < 3; i++ { // ricerca, poi inseguimento se c'è un viso
		f.seq = uint64(i + 1)
		f.captured = time.Now()
		eng.handle(f)
	}
	var snap Snapshot
	if !eng.Snapshot(&snap) {
		t.Fatal("nessun frame nuovo nello snapshot")
	}
	if snap.PixW != f.W || len(snap.Pix) != f.W*f.H || !snap.Overlay || snap.PixSeq != 3 {
		t.Fatalf("snapshot %d×%d, %d pixel, frame %d, sovrapposizioni %v", snap.PixW, snap.PixH, len(snap.Pix), snap.PixSeq, snap.Overlay)
	}
	if eng.Snapshot(&snap) {
		t.Fatal("frame nuovo senza nuove elaborazioni")
	}
	// modello indietro di due frame: l'anteprima passa al frame catturato,
	// senza sovrapposizioni
	eng.rawPix, eng.rawSeq = append([]byte(nil), f.Pix...), 5
	if !eng.Snapshot(&snap) || snap.Overlay || snap.PixSeq != 5 {
		t.Fatalf("anteprima diretta: frame %d, sovrapposizioni %v", snap.PixSeq, snap.Overlay)
	}
	eng.rawSeq = 0
	t.Logf("viso %v, probabilità %.3f, %s", snap.Res.Face, snap.Res.Score, snap.Res.Mode)

	out := os.Getenv("WEBCAM_FACE_PNG")
	for _, sz := range []image.Point{{1920, 1080}, {1280, 720}, {960, 540}, {300, 200}} {
		for _, mirror := range []bool{true, false} {
			a := NewApp(nil, eng, sz.X, sz.Y, mirror)
			if err := a.resize(sz.X, sz.Y); err != nil {
				t.Fatal(err)
			}
			for mode := 0; mode < meshModes; mode++ {
				a.meshMode = mode
				a.full = true
				a.paint(time.Now())
			}
			if out != "" && mirror {
				a.meshMode = meshContours
				a.full = true
				a.paint(time.Now())
				name := filepath.Join(out, "webcam-face-"+itoa(sz.X)+"x"+itoa(sz.Y)+".png")
				if err := savePNG(name, a.canvas); err != nil {
					t.Fatal(err)
				}
				t.Log("salvata", name)
			}
		}
	}
}

func TestComputeMetricsSynthetic(t *testing.T) {
	// occhio aperto ideale: angoli a distanza 30, palpebre a ±5 → EAR 1/3
	var lm headpose.Landmarks
	e := eyeLM[1]
	set := func(i int, x, y float64) { lm[i] = [3]float64{x, y, 0} }
	set(e.ear[0], 100, 100)
	set(e.ear[3], 130, 100)
	set(e.ear[1], 110, 95)
	set(e.ear[5], 110, 105)
	set(e.ear[2], 120, 95)
	set(e.ear[4], 120, 105)
	if ear := earOf(&lm, e.ear); math.Abs(ear-1.0/3) > 1e-9 {
		t.Fatalf("EAR %.4f, atteso 0,3333", ear)
	}
}
