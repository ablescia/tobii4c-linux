//go:build linux

package webcam

import (
	"testing"
	"unsafe"
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
		{"VIDIOC_S_CTRL", vidiocSCtrl, 0xc008561c},
		{"VIDIOC_QUERYCTRL", vidiocQueryCtrl, 0xc0445624},
		{"VIDIOC_QUERYMENU", vidiocQueryMenu, 0xc02c5625},
		{"VIDIOC_ENUM_FRAMESIZES", vidiocEnumFrameSizes, 0xc02c564a},
		{"VIDIOC_ENUM_FRAMEINTERVALS", vidiocEnumFrameIntervals, 0xc034564b},
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
	if err := toGray(PixYUYV, data, w, h, bpl, dst); err != nil {
		t.Fatal(err)
	}
	want := []byte{10, 20, 30, 40, 50, 60, 70, 80}
	if string(dst) != string(want) {
		t.Fatalf("luminanza %v, attesa %v", dst, want)
	}
	if err := toGray(PixYUYV, data[:12], w, h, bpl, dst); err == nil {
		t.Fatal("frame troncato accettato")
	}
}

// c925eModes sono le modalità della Logitech C925e (v4l2-ctl --list-formats-ext),
// più un formato che non si sa convertire.
func c925eModes() []Mode {
	iv := func(fps ...int) []v4l2Fract {
		var out []v4l2Fract
		for _, f := range fps {
			out = append(out, v4l2Fract{1, uint32(f)})
		}
		return out
	}
	all := iv(30, 24, 20, 15, 10)
	return []Mode{
		{PixYUYV, 640, 480, all}, {PixYUYV, 160, 90, all}, {PixYUYV, 800, 448, all},
		{PixYUYV, 864, 480, iv(24, 20)}, {PixYUYV, 960, 720, iv(15, 10)}, {PixYUYV, 1280, 720, iv(10)},
		{PixYUYV, 1920, 1080, iv(5)}, {PixYUYV, 2304, 1536, iv(2)},
		{PixMJPG, 640, 480, all}, {PixMJPG, 960, 720, all}, {PixMJPG, 1024, 576, all},
		{PixMJPG, 1280, 720, iv(60, 30, 24, 20, 15, 10)}, {PixMJPG, 1920, 1080, all},
		{fourcc("H264"), 1920, 1080, iv(60)},
	}
}

func TestPickMode(t *testing.T) {
	modes := c925eModes()
	for _, c := range []struct {
		name string
		req  ModeRequest
		pix  uint32
		w, h int
		fps  float64
	}{
		{"automatica: la più veloce", ModeRequest{}, PixMJPG, 1280, 720, 60},
		{"30 fps: la più grande", ModeRequest{FPS: 30}, PixMJPG, 1920, 1080, 30},
		{"frequenza vicina", ModeRequest{FPS: 50}, PixMJPG, 1280, 720, 60},
		{"640×480: YUYV senza decodifica", ModeRequest{W: 640, H: 480}, PixYUYV, 640, 480, 30},
		{"solo YUYV", ModeRequest{PixFmt: PixYUYV}, PixYUYV, 800, 448, 30},
		{"risoluzione vicina", ModeRequest{W: 1000, H: 700}, PixMJPG, 960, 720, 30},
		{"YUYV 1280×720", ModeRequest{W: 1280, H: 720, PixFmt: PixYUYV}, PixYUYV, 1280, 720, 10},
		{"720p a 24 fps", ModeRequest{W: 1280, H: 720, FPS: 24}, PixMJPG, 1280, 720, 24},
	} {
		m, iv, ok := pickMode(modes, c.req)
		if !ok || m.PixFmt != c.pix || m.W != c.w || m.H != c.h || fractFPS(iv) != c.fps {
			t.Errorf("%s: %s %d×%d a %v fps (trovata %v), attesa %s %d×%d a %v fps", c.name, FourCCString(m.PixFmt), m.W, m.H,
				fractFPS(iv), ok, FourCCString(c.pix), c.w, c.h, c.fps)
		}
	}
	if _, _, ok := pickMode(modes, ModeRequest{PixFmt: PixGREY}); ok {
		t.Error("GREY scelto su una webcam che non lo ha")
	}
	if _, _, ok := pickMode(nil, ModeRequest{}); ok {
		t.Error("modalità scelta senza modalità")
	}
}

func TestControlNames(t *testing.T) {
	for in, want := range map[string]string{
		"Exposure Time, Absolute":     "exposure_time_absolute",
		"White Balance, Automatic":    "white_balance_automatic",
		" Power Line Frequency ":      "power_line_frequency",
		"Exposure, Dynamic Framerate": "exposure_dynamic_framerate",
	} {
		if got := ctrlKey(in); got != want {
			t.Errorf("ctrlKey(%q) = %q, atteso %q", in, got, want)
		}
	}
	cs := []Control{
		{ID: 0x00980918, Name: "Power Line Frequency", Type: CtrlTypeMenu, Max: 2, Menu: map[int32]string{0: "Disabled", 1: "50 Hz", 2: "60 Hz"}},
		{ID: 0x00980900, Name: "Brightness", Type: CtrlTypeInteger, Max: 255},
		{ID: cidExposureAutoPriority, Name: "Exposure, Dynamic Framerate", Type: CtrlTypeBoolean, Max: 1},
	}
	for _, c := range []struct {
		name, value string
		want        int32
	}{
		{"power_line_frequency", "50_hz", 1},
		{"power_line_frequency", "60 Hz", 2},
		{"0x00980918", "0", 0},
		{"brightness", "200", 200},
		{"exposure_dynamic_framerate", "off", 0},
		{"Exposure, Dynamic Framerate", "on", 1},
	} {
		ct := findCtrl(cs, c.name)
		if ct == nil {
			t.Fatalf("controllo %q non trovato", c.name)
		}
		v, err := parseCtrlValue(ct, c.value)
		if err != nil || v != c.want {
			t.Errorf("%s=%s: %d, %v; atteso %d", c.name, c.value, v, err, c.want)
		}
	}
	for _, bad := range []struct{ name, value string }{{"brightness", "300"}, {"brightness", "tanta"}, {"power_line_frequency", "40_hz"}} {
		if _, err := parseCtrlValue(findCtrl(cs, bad.name), bad.value); err == nil {
			t.Errorf("%s=%s accettato", bad.name, bad.value)
		}
	}
	if findCtrl(cs, "zoom") != nil {
		t.Error("trovato un controllo che non c'è")
	}
	set, err := ParseCtrlSettings(" power_line_frequency=50_hz, brightness = 140 ,")
	if err != nil || len(set) != 2 || set[1] != (CtrlSetting{"brightness", "140"}) {
		t.Errorf("ParseCtrlSettings: %v, %v", set, err)
	}
	if _, err := ParseCtrlSettings("brightness"); err == nil {
		t.Error("controllo senza valore accettato")
	}
}
