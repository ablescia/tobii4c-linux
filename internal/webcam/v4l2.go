//go:build linux

package webcam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Acquisizione V4L2 in Go puro: ioctl e mmap, nessuna libreria C. Le
// strutture ricalcano <linux/videodev2.h> con la stessa disposizione in
// memoria; i numeri degli ioctl derivano dalle loro dimensioni (i test li
// confrontano con i valori del kernel a 64 bit).

const (
	bufTypeVideoCapture = 1
	memoryMMAP          = 1

	capVideoCapture = 0x00000001
	capStreaming    = 0x04000000
	capDeviceCaps   = 0x80000000

	BufFlagError              = 0x00000040
	bufFlagTimestampMask      = 0x0000e000
	bufFlagTimestampMonotonic = 0x00002000

	ctrlFlagNextCtrl  = 0x80000000
	CtrlFlagDisabled  = 0x0001
	CtrlFlagInactive  = 0x0010
	ctrlFlagWriteOnly = 0x0040

	CtrlTypeInteger     = 1
	CtrlTypeBoolean     = 2
	CtrlTypeMenu        = 3
	CtrlTypeButton      = 4
	CtrlTypeInteger64   = 5
	CtrlTypeCtrlClass   = 6
	CtrlTypeString      = 7
	CtrlTypeIntegerMenu = 9

	frmTypeDiscrete = 1 // V4L2_FRMSIZE_TYPE_DISCRETE, V4L2_FRMIVAL_TYPE_DISCRETE

	// V4L2_CID_EXPOSURE_AUTO_PRIORITY (exposure_dynamic_framerate): con
	// l'esposizione automatica la webcam può allungare la posa e scendere
	// sotto la frequenza chiesta (la C925e da 60 a 30 fps in una stanza).
	cidExposureAutoPriority = 0x009a0903
)

func fourcc(s string) uint32 {
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
}

var (
	PixYUYV = fourcc("YUYV")
	PixMJPG = fourcc("MJPG")
	PixGREY = fourcc("GREY")
)

func FourCCString(v uint32) string {
	b := []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
	return strings.TrimRight(string(b), " \x00")
}

type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2FmtDesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

type v4l2PixFormat struct {
	Width, Height, PixelFormat, Field, BytesPerLine, SizeImage uint32
	Colorspace, Priv, Flags, YcbcrEnc, Quantization, XferFunc  uint32
}

// v4l2Format: il tipo e un'unione di 200 byte allineata come un puntatore
// (l'unione contiene struct v4l2_window, che ha dei puntatori).
type v4l2Format struct {
	Type uint32
	fmt  [200 / unsafe.Sizeof(uintptr(0))]uintptr
}

func (f *v4l2Format) pix() *v4l2PixFormat { return (*v4l2PixFormat)(unsafe.Pointer(&f.fmt)) }

type v4l2RequestBuffers struct {
	Count, Type, Memory, Capabilities uint32
	Flags                             uint8
	Reserved                          [3]uint8
}

type v4l2Timecode struct {
	Type, Flags                     uint32
	Frames, Seconds, Minutes, Hours uint8
	UserBits                        [4]uint8
}

type v4l2Buffer struct {
	Index, Type, BytesUsed, Flags, Field uint32
	Timestamp                            syscall.Timeval
	Timecode                             v4l2Timecode
	Sequence, Memory                     uint32
	M                                    uintptr // unione: offset (u32) con V4L2_MEMORY_MMAP
	Length, Reserved2                    uint32
	RequestFD                            int32
}

// offset legge il membro offset dell'unione m (sta all'inizio dell'unione).
func (b *v4l2Buffer) offset() uint32 { return *(*uint32)(unsafe.Pointer(&b.M)) }

type v4l2Fract struct{ Numerator, Denominator uint32 }

type v4l2CaptureParm struct {
	Capability, CaptureMode   uint32
	TimePerFrame              v4l2Fract
	ExtendedMode, ReadBuffers uint32
	Reserved                  [4]uint32
}

type v4l2StreamParm struct {
	Type    uint32
	Capture v4l2CaptureParm
	_       [200 - unsafe.Sizeof(v4l2CaptureParm{})]byte
}

type v4l2QueryCtrl struct {
	ID, Type                             uint32
	Name                                 [32]byte
	Minimum, Maximum, Step, DefaultValue int32
	Flags                                uint32
	Reserved                             [2]uint32
}

type v4l2Control struct {
	ID    uint32
	Value int32
}

// v4l2FrmSizeEnum: U è l'unione discreta (larghezza, altezza) o a passi
// (min, max e passo della larghezza, poi dell'altezza).
type v4l2FrmSizeEnum struct {
	Index, PixelFormat, Type uint32
	U                        [6]uint32
	Reserved                 [2]uint32
}

// v4l2FrmIvalEnum: U è l'unione discreta (una frazione) o a passi (minimo,
// massimo e passo).
type v4l2FrmIvalEnum struct {
	Index, PixelFormat, Width, Height, Type uint32
	U                                       [6]uint32
	Reserved                                [2]uint32
}

// v4l2QueryMenu è impaccata nel kernel: 44 byte, il nome è in unione con il
// valore s64 dei menu interi.
type v4l2QueryMenu struct {
	ID, Index uint32
	Name      [32]byte
	Reserved  uint32
}

func ioc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'V'<<8 | nr }

const (
	iocWrite = 1
	iocRead  = 2
)

var (
	vidiocQueryCap  = ioc(iocRead, 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt   = ioc(iocRead|iocWrite, 2, unsafe.Sizeof(v4l2FmtDesc{}))
	vidiocGFmt      = ioc(iocRead|iocWrite, 4, unsafe.Sizeof(v4l2Format{}))
	vidiocSFmt      = ioc(iocRead|iocWrite, 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqBufs   = ioc(iocRead|iocWrite, 8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQueryBuf  = ioc(iocRead|iocWrite, 9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQBuf      = ioc(iocRead|iocWrite, 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBuf     = ioc(iocRead|iocWrite, 17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn  = ioc(iocWrite, 18, unsafe.Sizeof(int32(0)))
	vidiocStreamOff = ioc(iocWrite, 19, unsafe.Sizeof(int32(0)))
	vidiocGParm     = ioc(iocRead|iocWrite, 21, unsafe.Sizeof(v4l2StreamParm{}))
	vidiocSParm     = ioc(iocRead|iocWrite, 22, unsafe.Sizeof(v4l2StreamParm{}))
	vidiocGCtrl     = ioc(iocRead|iocWrite, 27, unsafe.Sizeof(v4l2Control{}))
	vidiocSCtrl     = ioc(iocRead|iocWrite, 28, unsafe.Sizeof(v4l2Control{}))
	vidiocQueryCtrl = ioc(iocRead|iocWrite, 36, unsafe.Sizeof(v4l2QueryCtrl{}))
	vidiocQueryMenu = ioc(iocRead|iocWrite, 37, unsafe.Sizeof(v4l2QueryMenu{}))

	vidiocEnumFrameSizes     = ioc(iocRead|iocWrite, 74, unsafe.Sizeof(v4l2FrmSizeEnum{}))
	vidiocEnumFrameIntervals = ioc(iocRead|iocWrite, 75, unsafe.Sizeof(v4l2FrmIvalEnum{}))
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		switch e {
		case 0:
			return nil
		case syscall.EINTR:
			continue
		default:
			return e
		}
	}
}

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// monotonicNow è CLOCK_MONOTONIC, l'orologio dei timestamp dei buffer V4L2.
func monotonicNow() time.Duration {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0)
	return time.Duration(ts.Nano())
}

// USBInfo sono gli attributi sysfs del dispositivo USB della webcam.
type USBInfo struct {
	ID           string // vid:pid
	Manufacturer string
	Product      string
	Serial       string
	Speed        string // Mbit/s
	Port         string // es. 1-9
	BCD          string // revisione del dispositivo
}

// CamInfo è la descrizione statica della webcam aperta.
type CamInfo struct {
	Path         string
	Driver       string
	Card         string
	Bus          string
	Version      string
	DevCaps      uint32
	W, H         int
	PixFmt       uint32
	BytesPerLine int
	SizeImage    int
	Colorspace   uint32
	Quantization uint32
	XferFunc     uint32
	YcbcrEnc     uint32
	FPS          float64 // frequenza nominale (VIDIOC_G_PARM)
	Formats      []string
	Buffers      int
	BufferBytes  int
	USB          USBInfo
	Modes        []Mode       // modalità elencate dalla webcam
	Choice       string       // come è stata scelta la modalità
	Changed      []CtrlChange // controlli cambiati all'apertura (ripristinati da Close)
}

// Webcam è un dispositivo V4L2 in streaming con buffer mmap.
type Webcam struct {
	Info CamInfo

	fd   int
	bufs [][]byte
	on   bool
}

// OpenOptions dice come aprire la webcam.
type OpenOptions struct {
	Mode    ModeRequest
	Buffers int
	// FixedFPS spegne exposure_dynamic_framerate: la frequenza resta
	// quella chiesta anche con poca luce (la posa si accorcia, sale il
	// guadagno).
	FixedFPS bool
	Controls []CtrlSetting // impostati dopo il formato, nell'ordine
}

// Open apre path, sceglie la modalità (pickMode tra quelle elencate
// dalla webcam), imposta formato, frequenza e controlli e avvia lo
// streaming. La modalità effettiva è in Info.
func Open(path string, opt OpenOptions) (*Webcam, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c := &Webcam{fd: fd}
	c.Info.Path = path
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	var cp v4l2Capability
	if err := ioctl(fd, vidiocQueryCap, unsafe.Pointer(&cp)); err != nil {
		return nil, fmt.Errorf("%s: VIDIOC_QUERYCAP: %w (non è un dispositivo V4L2?)", path, err)
	}
	in := &c.Info
	in.Driver, in.Card, in.Bus = cstr(cp.Driver[:]), cstr(cp.Card[:]), cstr(cp.BusInfo[:])
	in.Version = fmt.Sprintf("%d.%d.%d", cp.Version>>16, cp.Version>>8&0xff, cp.Version&0xff)
	in.DevCaps = cp.Capabilities
	if cp.Capabilities&capDeviceCaps != 0 {
		in.DevCaps = cp.DeviceCaps
	}
	if in.DevCaps&capVideoCapture == 0 || in.DevCaps&capStreaming == 0 {
		return nil, fmt.Errorf("%s (%s): non è un nodo di cattura video in streaming", path, in.Card)
	}
	in.Formats = enumFormats(fd)
	in.USB = usbInfo(path)
	in.Modes = enumModes(fd)

	req := opt.Mode
	w, h, pix := req.W, req.H, req.PixFmt
	var ival v4l2Fract
	if m, iv, ok := pickMode(in.Modes, req); ok {
		w, h, pix, ival = m.W, m.H, m.PixFmt, iv
		in.Choice = choiceText(req, len(in.Modes))
	} else {
		// modalità non elencate (o nessuna convertibile): si chiede quella
		// data e il driver sceglie la più vicina
		if w <= 0 || h <= 0 {
			w, h = 640, 480
		}
		if pix == 0 {
			pix = PixYUYV
		}
		ival = v4l2Fract{1, 30}
		if req.FPS > 0 {
			ival = v4l2Fract{1000, uint32(math.Round(req.FPS * 1000))}
		}
		in.Choice = "chiesta al driver (modalità non elencate)"
	}

	var f v4l2Format
	f.Type = bufTypeVideoCapture
	p := f.pix()
	p.Width, p.Height, p.PixelFormat = uint32(w), uint32(h), pix
	if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("%s: la webcam è già in uso da un altro programma", path)
		}
		return nil, fmt.Errorf("%s: VIDIOC_S_FMT: %w", path, err)
	}
	if p.PixelFormat != pix {
		return nil, fmt.Errorf("%s: formato %s non supportato (disponibili: %s)", path, FourCCString(pix), strings.Join(in.Formats, ", "))
	}
	in.W, in.H, in.PixFmt = int(p.Width), int(p.Height), p.PixelFormat
	in.BytesPerLine, in.SizeImage = int(p.BytesPerLine), int(p.SizeImage)
	in.Colorspace, in.Quantization, in.XferFunc, in.YcbcrEnc = p.Colorspace, p.Quantization, p.XferFunc, p.YcbcrEnc

	// frequenza: S_PARM può fallire (non tutte le webcam la regolano)
	if ival.Numerator > 0 && ival.Denominator > 0 {
		sp := v4l2StreamParm{Type: bufTypeVideoCapture}
		sp.Capture.TimePerFrame = ival
		_ = ioctl(fd, vidiocSParm, unsafe.Pointer(&sp))
	}
	sp := v4l2StreamParm{Type: bufTypeVideoCapture}
	if ioctl(fd, vidiocGParm, unsafe.Pointer(&sp)) == nil && sp.Capture.TimePerFrame.Numerator > 0 {
		in.FPS = fractFPS(sp.Capture.TimePerFrame)
	}

	if err := c.applyControls(opt); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	rb := v4l2RequestBuffers{Count: uint32(max(opt.Buffers, 2)), Type: bufTypeVideoCapture, Memory: memoryMMAP}
	if err := ioctl(fd, vidiocReqBufs, unsafe.Pointer(&rb)); err != nil {
		return nil, fmt.Errorf("%s: VIDIOC_REQBUFS: %w", path, err)
	}
	if rb.Count < 2 {
		return nil, fmt.Errorf("%s: il driver concede solo %d buffer", path, rb.Count)
	}
	for i := 0; i < int(rb.Count); i++ {
		b := v4l2Buffer{Index: uint32(i), Type: bufTypeVideoCapture, Memory: memoryMMAP}
		if err := ioctl(fd, vidiocQueryBuf, unsafe.Pointer(&b)); err != nil {
			return nil, fmt.Errorf("%s: VIDIOC_QUERYBUF: %w", path, err)
		}
		data, err := syscall.Mmap(fd, int64(b.offset()), int(b.Length), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return nil, fmt.Errorf("%s: mmap: %w", path, err)
		}
		c.bufs = append(c.bufs, data)
		in.BufferBytes = int(b.Length)
		if err := ioctl(fd, vidiocQBuf, unsafe.Pointer(&b)); err != nil {
			return nil, fmt.Errorf("%s: VIDIOC_QBUF: %w", path, err)
		}
	}
	in.Buffers = len(c.bufs)
	typ := int32(bufTypeVideoCapture)
	if err := ioctl(fd, vidiocStreamOn, unsafe.Pointer(&typ)); err != nil {
		return nil, fmt.Errorf("%s: VIDIOC_STREAMON: %w", path, err)
	}
	c.on = true
	ok = true
	return c, nil
}

func enumFormats(fd int) []string {
	var out []string
	for i := uint32(0); i < 64; i++ {
		d := v4l2FmtDesc{Index: i, Type: bufTypeVideoCapture}
		if ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)) != nil {
			break
		}
		out = append(out, FourCCString(d.PixelFormat))
	}
	return out
}

// BufInfo descrive un buffer pieno restituito dal driver.
type BufInfo struct {
	Sequence  uint32
	Flags     uint32
	Timestamp time.Duration // del driver (CLOCK_MONOTONIC se Flags lo dice)
	Dequeued  time.Duration // CLOCK_MONOTONIC al prelievo
}

// Monotonic dice se il timestamp del buffer è su CLOCK_MONOTONIC.
func (b BufInfo) Monotonic() bool {
	return b.Flags&bufFlagTimestampMask == bufFlagTimestampMonotonic
}

var ErrTimeout = errors.New("nessun frame dalla webcam")

// Next attende il frame successivo (al più timeout), chiama fn con i dati
// del buffer e lo restituisce al driver. data vale solo durante fn.
func (c *Webcam) Next(timeout time.Duration, fn func(data []byte, bi BufInfo)) error {
	deadline := time.Now().Add(timeout)
	for {
		b := v4l2Buffer{Type: bufTypeVideoCapture, Memory: memoryMMAP}
		err := ioctl(c.fd, vidiocDQBuf, unsafe.Pointer(&b))
		if err == syscall.EAGAIN {
			rem := time.Until(deadline)
			if rem <= 0 {
				return ErrTimeout
			}
			if err := waitReadable(c.fd, min(rem, 100*time.Millisecond)); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("VIDIOC_DQBUF: %w", err)
		}
		bi := BufInfo{Sequence: b.Sequence, Flags: b.Flags, Dequeued: monotonicNow(),
			Timestamp: time.Duration(b.Timestamp.Sec)*time.Second + time.Duration(b.Timestamp.Usec)*time.Microsecond}
		if int(b.Index) < len(c.bufs) {
			data := c.bufs[b.Index]
			fn(data[:min(int(b.BytesUsed), len(data))], bi)
		}
		if err := ioctl(c.fd, vidiocQBuf, unsafe.Pointer(&b)); err != nil {
			return fmt.Errorf("VIDIOC_QBUF: %w", err)
		}
		return nil
	}
}

func waitReadable(fd int, d time.Duration) error {
	const nfd = int(8 * unsafe.Sizeof(syscall.FdSet{}.Bits[0]))
	var set syscall.FdSet
	set.Bits[fd/nfd] |= 1 << (fd % nfd)
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	_, err := syscall.Select(fd+1, &set, nil, nil, &tv)
	if err == syscall.EINTR {
		return nil
	}
	return err
}

// Close rimette i controlli cambiati da Open com'erano, ferma lo
// streaming e rilascia buffer e descrittore.
func (c *Webcam) Close() {
	if c.fd >= 0 {
		for i := len(c.Info.Changed) - 1; i >= 0; i-- {
			ch := c.Info.Changed[i]
			if err := c.restoreCtrl(ch); err != nil {
				fmt.Fprintf(os.Stderr, "webcam-face: %s non ripristinato a %d (v4l2-ctl -c %s=%d): %v\n",
					ch.Name, ch.Old, ch.Name, ch.Old, err)
			}
		}
		c.Info.Changed = nil
	}
	if c.on {
		typ := int32(bufTypeVideoCapture)
		_ = ioctl(c.fd, vidiocStreamOff, unsafe.Pointer(&typ))
		c.on = false
	}
	for _, b := range c.bufs {
		_ = syscall.Munmap(b)
	}
	c.bufs = nil
	if c.fd >= 0 {
		syscall.Close(c.fd)
		c.fd = -1
	}
}

// restoreCtrl rimette un controllo al valore originale e lo rilegge; un
// trasferimento di controllo USB può fallire per un attimo, quindi riprova.
func (c *Webcam) restoreCtrl(ch CtrlChange) error {
	var err error
	for try := 0; try < 5; try++ {
		if try > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		v := v4l2Control{ID: ch.ID, Value: ch.Old}
		if err = ioctl(c.fd, vidiocSCtrl, unsafe.Pointer(&v)); err != nil {
			continue
		}
		g := v4l2Control{ID: ch.ID}
		if err = ioctl(c.fd, vidiocGCtrl, unsafe.Pointer(&g)); err == nil {
			if g.Value == ch.Old {
				return nil
			}
			err = fmt.Errorf("riletto %d", g.Value)
		}
	}
	return err
}

// Control è un controllo V4L2 (luminosità, esposizione, …).
type Control struct {
	ID                      uint32
	Name                    string
	Type                    uint32
	Min, Max, Step, Default int32
	Flags                   uint32
	Value                   int32
	OK                      bool // Value è stato letto
	Menu                    map[int32]string
	Class                   bool // intestazione di un gruppo di controlli
}

// Controls elenca i controlli della webcam con i valori attuali.
func (c *Webcam) Controls() []Control {
	var out []Control
	id := uint32(ctrlFlagNextCtrl)
	for len(out) < 256 {
		q := v4l2QueryCtrl{ID: id}
		if ioctl(c.fd, vidiocQueryCtrl, unsafe.Pointer(&q)) != nil {
			break
		}
		id = q.ID | ctrlFlagNextCtrl
		if q.Flags&CtrlFlagDisabled != 0 {
			continue
		}
		ct := Control{ID: q.ID, Name: cstr(q.Name[:]), Type: q.Type, Min: q.Minimum, Max: q.Maximum,
			Step: q.Step, Default: q.DefaultValue, Flags: q.Flags, Class: q.Type == CtrlTypeCtrlClass}
		if q.Type == CtrlTypeMenu || q.Type == CtrlTypeIntegerMenu {
			ct.Menu = map[int32]string{}
			for i := q.Minimum; i <= q.Maximum && i-q.Minimum < 64; i++ {
				m := v4l2QueryMenu{ID: q.ID, Index: uint32(i)}
				if ioctl(c.fd, vidiocQueryMenu, unsafe.Pointer(&m)) != nil {
					continue
				}
				if q.Type == CtrlTypeMenu {
					ct.Menu[i] = cstr(m.Name[:])
				} else {
					ct.Menu[i] = strconv.FormatInt(int64(binary.NativeEndian.Uint64(m.Name[:8])), 10)
				}
			}
		}
		out = append(out, ct)
	}
	c.ReadControls(out)
	return out
}

// ReadControls aggiorna i valori dei controlli leggibili.
func (c *Webcam) ReadControls(cs []Control) {
	for i := range cs {
		ct := &cs[i]
		switch {
		case ct.Class, ct.Flags&ctrlFlagWriteOnly != 0, ct.Type == CtrlTypeButton,
			ct.Type == CtrlTypeString, ct.Type == CtrlTypeInteger64:
			continue
		}
		v := v4l2Control{ID: ct.ID}
		ct.OK = ioctl(c.fd, vidiocGCtrl, unsafe.Pointer(&v)) == nil
		if ct.OK {
			ct.Value = v.Value
		}
	}
}

// ---------------------------------------------------------------------------
// Ricerca del dispositivo in sysfs

// videoNodes restituisce i nodi /dev/videoN in ordine numerico.
func videoNodes() []string {
	paths, _ := filepath.Glob("/sys/class/video4linux/video*")
	num := func(p string) int {
		n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(p), "video"))
		return n
	}
	sort.Slice(paths, func(i, j int) bool { return num(paths[i]) < num(paths[j]) })
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "/dev/" + filepath.Base(p)
	}
	return out
}

// usbDir è la cartella sysfs del dispositivo USB a cui appartiene il nodo.
func usbDir(dev string) string {
	d, err := filepath.EvalSymlinks(filepath.Join("/sys/class/video4linux", filepath.Base(dev), "device"))
	if err != nil {
		return ""
	}
	// il nodo punta all'interfaccia (es. 1-9:1.0): il dispositivo è il padre
	for i := 0; i < 3 && d != "/"; i++ {
		if _, err := os.Stat(filepath.Join(d, "idVendor")); err == nil {
			return d
		}
		d = filepath.Dir(d)
	}
	return ""
}

func readAttr(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func usbInfo(dev string) USBInfo {
	d := usbDir(dev)
	if d == "" {
		return USBInfo{}
	}
	return USBInfo{
		ID:           readAttr(d, "idVendor") + ":" + readAttr(d, "idProduct"),
		Manufacturer: readAttr(d, "manufacturer"),
		Product:      readAttr(d, "product"),
		Serial:       readAttr(d, "serial"),
		Speed:        readAttr(d, "speed"),
		Port:         filepath.Base(d),
		BCD:          readAttr(d, "bcdDevice"),
	}
}

// isCaptureNode dice se dev è un nodo di cattura video (le webcam UVC hanno
// anche un nodo di soli metadati).
func isCaptureNode(dev string) bool {
	fd, err := syscall.Open(dev, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	var cp v4l2Capability
	if ioctl(fd, vidiocQueryCap, unsafe.Pointer(&cp)) != nil {
		return false
	}
	caps := cp.Capabilities
	if caps&capDeviceCaps != 0 {
		caps = cp.DeviceCaps
	}
	return caps&capVideoCapture != 0 && caps&capStreaming != 0
}

// FindWebcam cerca il nodo di cattura della prima webcam USB con uno degli
// id vid:pid della lista (separati da virgole, in ordine di preferenza); se
// non c'è, ripiega sulla prima webcam trovata.
func FindWebcam(ids string) (string, error) {
	var first string
	found := map[string]string{} // vid:pid → nodo
	for _, n := range videoNodes() {
		if !isCaptureNode(n) {
			continue
		}
		if first == "" {
			first = n
		}
		if d := usbDir(n); d != "" {
			id := strings.ToLower(readAttr(d, "idVendor") + ":" + readAttr(d, "idProduct"))
			if found[id] == "" {
				found[id] = n
			}
		}
	}
	for _, id := range strings.Split(ids, ",") {
		if n := found[strings.ToLower(strings.TrimSpace(id))]; n != "" {
			return n, nil
		}
	}
	if first != "" {
		fmt.Fprintf(os.Stderr, "webcam-face: nessuna webcam USB %s, uso %s\n", ids, first)
		return first, nil
	}
	return "", errors.New("nessuna webcam V4L2 trovata (/dev/video*)")
}
