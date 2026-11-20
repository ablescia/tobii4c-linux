// tobii4c: driver user-space per il Tobii Eye Tracker 4C su Linux.
//
//	tobii4c run [flag]                stream di sguardo + testa verso UDP / LSL
//	tobii4c calibrate [flag]          configurazione e calibrazione dello sguardo (X11)
//	tobii4c headpose-calibrate [flag] calibrazione della camera per il pitch della testa (X11)
//	tobii4c calibration save|apply|restore
//	tobii4c display [flag]            legge o imposta la display area
//	tobii4c info [flag]               identità, catalogo stream, colonne, display area
//	tobii4c recv [flag]               ricevitore UDP di prova
//	tobii4c udev                      stampa la regola udev da installare
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/gousb"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/sink"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/usbdev"
	"github.com/ablescia/tobii4c-linux/udev"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "info":
		err = cmdInfo(args)
	case "recv":
		err = cmdRecv(args)
	case "calibrate":
		err = cmdCalibrate(args)
	case "calibration":
		err = cmdCalibration(args)
	case "display":
		err = cmdDisplay(args)
	case "headpose-calibrate":
		err = cmdHeadPoseCalibrate(args)
	case "lsl-recv":
		err = cmdLSLRecv(args)
	case "udev":
		fmt.Print(udev.Rules)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "comando sconosciuto %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "errore:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tobii4c — driver user-space per Tobii Eye Tracker 4C (2104:0127)

uso: tobii4c <comando> [flag]

  run          stream di sguardo e testa verso UDP (default 127.0.0.1:5005) e/o LSL
  calibrate    configurazione a schermo intero (X11): posizionamento del sensore,
               posizione dell'utente, calibrazione a 7 punti, verifica
  headpose-calibrate
               app a schermo intero per il pitch della testa: calibrazione della
               camera con fasi guidate a tempo, zero del viso, verifica
  calibration  save|apply|restore del blob di calibrazione
  display      legge o imposta la display area (geometria dello schermo)
  info         interroga il dispositivo: identità, stream, colonne, display area
  recv         ricevitore UDP di prova per il formato T4CG (v1 e v2)
  lsl-recv     ricevitore LSL di prova (build con -tags lsl)
  udev         stampa la regola udev (vedi README per l'installazione)

tobii4c <comando> -h per i flag.
`)
}

// common raccoglie i flag condivisi.
type common struct {
	logLevel, logFormat string
	pid                 string
	transferSize        int
	inFlight            int
	reqTimeout          time.Duration
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.logLevel, "log-level", "info", "debug|info|warn|error")
	fs.StringVar(&c.logFormat, "log-format", "text", "text|json")
	fs.StringVar(&c.pid, "pid", "", "product id esadecimale da usare (default: 0127 poi 0313)")
	fs.IntVar(&c.transferSize, "transfer-size", 65536, "byte per bulk transfer IN (multiplo di 512)")
	fs.IntVar(&c.inFlight, "in-flight", 16, "bulk transfer IN tenute in coda nel kernel")
	fs.DurationVar(&c.reqTimeout, "request-timeout", 1500*time.Millisecond, "timeout di una richiesta TTP")
}

func (c *common) logger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if c.logFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func (c *common) usb() (usbdev.Config, error) {
	u := usbdev.DefaultConfig()
	u.TransferSize, u.InFlight = c.transferSize, c.inFlight
	if u.TransferSize <= 0 || u.TransferSize%512 != 0 {
		return u, fmt.Errorf("-transfer-size deve essere un multiplo positivo di 512")
	}
	if c.pid != "" {
		v, err := strconv.ParseUint(strings.TrimPrefix(c.pid, "0x"), 16, 16)
		if err != nil {
			return u, fmt.Errorf("-pid: %w", err)
		}
		u.ProductIDs = []gousb.ID{gousb.ID(v)}
	}
	return u, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ---------------------------------------------------------------------------

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	udpAddr := fs.String("udp", "127.0.0.1:5005", "destinazione UDP dei campioni (vuoto = disattivato)")
	useLSL := fs.Bool("lsl", false, "pubblica un outlet Lab Streaming Layer (build con -tags lsl)")
	lslName := fs.String("lsl-name", "Tobii4C", "nome dell'outlet LSL")
	printEvery := fs.Duration("print", 0, "stampa un campione su stdout ogni intervallo (0 = no)")
	workers := fs.Int("workers", 2, "goroutine di decodifica")
	rawQueue := fs.Int("raw-queue", 1, "capacità del canale reader→framer (0 = non bufferizzato)")
	framePool := fs.Int("frame-pool", 64, "frame in volo nella pipeline prima di scartare")
	presence := fs.Bool("presence", true, "sottoscrive lo stream presence (0x504) e lo riporta in ogni campione")
	stall := fs.Duration("stall", 3*time.Second, "senza frame per questo tempo: resubscribe, poi riconnessione")
	keepAlive := fs.Duration("keepalive", 5*time.Second, "ping periodico di salute (0 = disattivato)")
	stats := fs.Duration("stats", 10*time.Second, "log periodico delle statistiche (0 = no)")
	dispMM := fs.String("display-mm", "", "imposta la display area a ogni connessione: LARGHEZZAxALTEZZA in mm (es. 698x393), calcolata col mounting del tracker; \"saved\" = quella di tobii4c calibrate (~/.config/tobii4c/display.json, se c'è)")
	dispX := fs.Float64("display-offset-x", 0, "mm di cui il centro dello schermo sta a destra del centro del tracker")
	calFile := fs.String("calibration", "", "applica questo blob di calibrazione a ogni connessione; \"saved\" = ~/.config/tobii4c/calibration.bin, se c'è")
	var hp headposeFlags
	hp.register(fs)
	fs.Parse(args)

	log := c.logger()
	usbConf, err := c.usb()
	if err != nil {
		return err
	}
	disp, err := loadDisplayArg(*dispMM, *dispX, log)
	if err != nil {
		return err
	}
	calBlob, err := loadCalibrationArg(*calFile, log)
	if err != nil {
		return err
	}

	var sinks []tracker.Sink
	if *udpAddr != "" {
		u, err := sink.NewUDP(*udpAddr)
		if err != nil {
			return fmt.Errorf("udp: %w", err)
		}
		sinks = append(sinks, u)
		log.Info("udp output", "addr", *udpAddr, "packet_bytes", sink.PacketSize, "format", fmt.Sprintf("T4CG v%d little-endian", sink.PacketVersion))
	}
	if *useLSL {
		l, err := sink.NewLSL(*lslName, "tobii4c", 90)
		if err != nil {
			return err
		}
		sinks = append(sinks, l)
		log.Info("lsl outlet", "name", *lslName)
	}
	if *printEvery > 0 {
		sinks = append(sinks, &sink.Print{W: os.Stdout, Every: *printEvery})
	}
	if len(sinks) == 0 {
		return fmt.Errorf("nessun output: usa -udp, -lsl o -print")
	}
	defer func() {
		for _, s := range sinks {
			s.Close()
		}
	}()

	est, err := hp.setup(log)
	if err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()
	opts := tracker.Options{
		USB: usbConf, Workers: *workers, RawQueue: *rawQueue, FramePool: *framePool,
		KeyMap: gaze.DefaultKeyMap(), DisplaySpec: disp, CalibrationBlob: calBlob, Presence: *presence,
		RequestTimeout: c.reqTimeout, StallTimeout: *stall, KeepAlive: *keepAlive,
		StatsInterval: *stats,
	}
	if est != nil {
		defer est.Close()
		opts.HeadPose = est
		opts.ImagesWhenAway = hp.always
		go logHeadPoseStats(ctx, est, *stats, log)
	}
	t := tracker.New(opts, log, sinks...)
	log.Info("tobii4c starting", "workers", *workers, "gomaxprocs", runtime.GOMAXPROCS(0))
	err = t.Run(ctx)
	log.Info("tobii4c stopped",
		"frames", t.Stats.GazeFrames.Load(), "host_drops", t.Stats.HostDrops.Load(),
		"device_drops", t.Stats.DeviceDrops.Load(), "reconnects", t.Stats.Reconnects.Load())
	return err
}

// ---------------------------------------------------------------------------

type infoReport struct {
	USB           usbdev.Info              `json:"usb"`
	DeviceStrings []string                 `json:"device_strings"`
	Streams       []streamReport           `json:"streams"`
	DisplayArea   *ttp.DisplayArea         `json:"display_area,omitempty"`
	Probes        map[string]probeResponse `json:"probes,omitempty"`
}

type streamReport struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Columns []columnInfo `json:"columns,omitempty"`
	Error   string       `json:"columns_error,omitempty"`
}

type columnInfo struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type probeResponse struct {
	Error   string   `json:"error,omitempty"`
	Strings []string `json:"strings,omitempty"`
	U32s    []string `json:"u32,omitempty"`
	Hex     string   `json:"hex,omitempty"`
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	var c common
	c.register(fs)
	jsonOut := fs.String("json", "", "scrive il report anche in questo file JSON")
	probe := fs.Bool("probe", false, "interroga anche op di sola lettura note (1330, 1400, 1490, 1650, 3170)")
	fs.Parse(args)
	log := c.logger()
	usbConf, err := c.usb()
	if err != nil {
		return err
	}
	var rep infoReport
	ready := func(s *tracker.Session) error {
		rep.USB = s.DeviceInfo()
		rep.DeviceStrings = s.DeviceStrings
		rep.DisplayArea = s.Display
		for _, r := range s.Catalogue {
			sr := streamReport{ID: fmt.Sprintf("0x%04x", r.ID), Name: r.Name}
			cols, err := s.StreamColumns(r.ID)
			if err != nil {
				sr.Error = err.Error()
			}
			for _, col := range cols {
				sr.Columns = append(sr.Columns, columnInfo{Key: fmt.Sprintf("0x%02x", col.ID), Name: col.Name})
			}
			rep.Streams = append(rep.Streams, sr)
		}
		if *probe {
			rep.Probes = map[string]probeResponse{}
			for _, op := range []uint32{ttp.OpProperties, ttp.OpTrackBox, ttp.OpStatus, ttp.OpOutputRate, ttp.OpGetEnabledEye, ttp.OpMounting} {
				_, p, err := s.Request(op, nil)
				pr := probeResponse{Strings: ttp.ParseStrings(p)}
				if err != nil {
					pr.Error = err.Error()
				}
				for _, v := range ttp.ParseU32s(p) {
					pr.U32s = append(pr.U32s, fmt.Sprintf("0x%x", v))
				}
				if len(p) > 0 {
					pr.Hex = fmt.Sprintf("%x", p[:min(len(p), 512)])
				}
				rep.Probes[fmt.Sprintf("0x%x", op)] = pr
			}
		}
		return nil
	}
	ctx, cancel := signalContext()
	defer cancel()
	opt := tracker.Options{USB: usbConf, RequestTimeout: c.reqTimeout, NoSubscribe: true, OnReady: ready,
		FailFast: true}
	ctx, cancelT := context.WithTimeout(ctx, 20*time.Second)
	defer cancelT()
	if err := tracker.New(opt, log).Run(ctx); err != nil {
		return err
	}
	if rep.DeviceStrings == nil && rep.Streams == nil {
		return fmt.Errorf("nessuna risposta dal dispositivo (timeout)")
	}
	printInfo(&rep)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			return err
		}
		fmt.Println("\nreport JSON:", *jsonOut)
	}
	return nil
}

func printInfo(r *infoReport) {
	fmt.Printf("USB      %s:%s bus %d addr %d, %s, %q %q serial %q\n",
		r.USB.VendorID, r.USB.ProductID, r.USB.Bus, r.USB.Address, r.USB.Speed, r.USB.Manufacturer, r.USB.Product, r.USB.Serial)
	labels := []string{"seriale", "modello", "generazione", "firmware"}
	for i, s := range r.DeviceStrings {
		l := fmt.Sprintf("stringa %d", i)
		if i < len(labels) {
			l = labels[i]
		}
		fmt.Printf("%-12s %s\n", l, s)
	}
	if r.DisplayArea != nil {
		fmt.Printf("display area %s\n", r.DisplayArea)
	}
	fmt.Printf("\nstream (%d):\n", len(r.Streams))
	for _, s := range r.Streams {
		fmt.Printf("  %s  %-24s", s.ID, s.Name)
		if s.Error != "" {
			fmt.Printf("  colonne: %s", s.Error)
		}
		fmt.Println()
		for _, c := range s.Columns {
			fmt.Printf("        %s  %s\n", c.Key, c.Name)
		}
	}
	if len(r.Probes) > 0 {
		keys := make([]string, 0, len(r.Probes))
		for k := range r.Probes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("\nprobe:")
		for _, k := range keys {
			p := r.Probes[k]
			fmt.Printf("  op %s: err=%q strings=%q u32=%v\n", k, p.Error, p.Strings, p.U32s)
		}
	}
}

// ---------------------------------------------------------------------------

func cmdRecv(args []string) error {
	fs := flag.NewFlagSet("recv", flag.ExitOnError)
	addr := fs.String("udp", "127.0.0.1:5005", "indirizzo di ascolto")
	every := fs.Duration("print", 200*time.Millisecond, "stampa al massimo un campione per intervallo (0 = tutti)")
	fs.Parse(args)
	la, err := net.ResolveUDPAddr("udp", *addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", la)
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "in ascolto su %s (Ctrl+C per uscire)\n", conn.LocalAddr())
	ctx, cancel := signalContext()
	defer cancel()
	go func() { <-ctx.Done(); conn.Close() }()

	buf := make([]byte, 2048)
	var s gaze.Sample
	var lastSeq uint64
	var n, gaps uint64
	var lastPrint, windowStart time.Time
	var windowN int
	for {
		k, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "\nricevuti %d campioni, %d buchi di seq\n", n, gaps)
				return nil
			}
			return err
		}
		if err := sink.Unmarshal(buf[:k], &s); err != nil {
			continue
		}
		n++
		if lastSeq != 0 && s.Seq != lastSeq+1 {
			gaps++
		}
		lastSeq = s.Seq
		now := time.Now()
		if windowStart.IsZero() {
			windowStart = now
		}
		windowN++
		if *every == 0 || now.Sub(lastPrint) >= *every {
			hz := float64(windowN) / now.Sub(windowStart).Seconds()
			age := float64(now.UnixNano()-s.HostTimeNs) / 1e3
			fmt.Printf("%s  %.1f Hz  età %.0fµs\n", sink.Format(&s), hz, age)
			lastPrint = now
			if now.Sub(windowStart) > 2*time.Second {
				windowStart, windowN = now, 0
			}
		}
	}
}
