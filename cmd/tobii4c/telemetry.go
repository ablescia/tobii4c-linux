package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/sink"
	"github.com/ablescia/tobii4c-linux/internal/telemetry"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// serviceUnit è l'unit utente installata dal README.
const serviceUnit = "tobii4c.service"

// cmdTelemetry apre la finestra di telemetria: l'anteprima della camera IR
// con tutti i punti del viso e, accanto, tutto ciò che il driver riceve dal
// 4C. Il driver gira dentro la finestra; se il servizio systemd tiene
// occupato il tracker, chiede di fermarlo e lo riavvia all'uscita.
func cmdTelemetry(args []string) error {
	fs := flag.NewFlagSet("telemetry", flag.ExitOnError)
	var c common
	c.register(fs)
	monName := fs.String("monitor", "", "monitor su cui aprire la finestra (default: il primario); vedi tobii4c calibrate -monitors")
	size := fs.String("size", "", "dimensione iniziale della finestra, LARGHEZZAxALTEZZA in pixel (default: quella del monitor)")
	maximized := fs.Bool("maximized", true, "chiede al window manager di aprire la finestra massimizzata (Omarchy ignora la richiesta: usa F o -fullscreen)")
	fullscreen := fs.Bool("fullscreen", false, "apre la finestra a schermo intero (il tasto F alterna)")
	udpAddr := fs.String("udp", "", "inoltra anche i campioni via UDP, es. 127.0.0.1:5005 (vuoto = no)")
	stopService := fs.Bool("stop-service", false, "ferma senza chiedere il servizio systemd che usa il tracker ("+serviceUnit+" o un altro che avvia il driver)")
	restart := fs.Bool("restart-service", true, "alla chiusura riavvia il servizio, se la telemetria lo ha fermato")
	fwLog := fs.Bool("firmware-log", true, "sottoscrive lo stream log del firmware (0x1772) e lo mostra nel registro")
	extra := fs.String("extra-streams", "", "altri stream da sottoscrivere e mostrare, es. 0x1770 (algodbg)")
	keymap := fs.String("keymap", "", "file JSON con la mappa colonne (vedi tobii4c keymap)")
	shots := fs.String("screenshots", ".", "cartella in cui il tasto S salva le schermate")
	keepAlive := fs.Duration("keepalive", 2*time.Second, "ping periodico di salute: ne mostra il tempo di risposta (0 = disattivato)")
	stats := fs.Duration("stats", 0, "log periodico delle statistiche su stderr (0 = no: sono già nella finestra)")
	simulate := fs.Bool("simulate", false, "nessun tracker: dati simulati (prova dell'interfaccia)")
	simImage := fs.String("simulate-image", "", "con -simulate: immagine (PGM salvato da dump -images, PNG o JPEG) passata al modello del viso a 30 Hz")
	autoShot := fs.Duration("autoshot", 0, "salva una schermata a questo intervallo nella cartella di -screenshots (0 = no)")
	duration := fs.Duration("duration", 0, "chiude la finestra dopo questo tempo (0 = mai)")
	var hp headposeFlags
	hp.register(fs)
	// nella telemetria l'anteprima è lo scopo: le immagini restano attive
	hp.always = true
	fs.Lookup("headpose-images-always").DefValue = "true"
	fs.Parse(args)

	km := gaze.DefaultKeyMap()
	if *keymap != "" {
		var err error
		if km, err = gaze.LoadKeyMap(*keymap); err != nil {
			return err
		}
	}
	col := telemetry.New(km)
	base := c.logger()
	log := slog.New(telemetry.NewLogHandler(base.Handler(), col))

	extraStreams, err := parseStreams(*extra)
	if err != nil {
		return err
	}
	if *fwLog && !slices.Contains(extraStreams, ttp.StreamLog) {
		extraStreams = append(extraStreams, ttp.StreamLog)
	}
	usbConf, err := c.usb()
	if err != nil {
		return err
	}

	// Un servizio che usa il tracker (tobii4c.service, o uno che avvia
	// `tobii4c run`, come tobii-ws) lo tiene in esclusiva: va fermato prima
	// di aprire la sessione USB, e riavviato all'uscita.
	var units []string
	if !*simulate {
		if unitActive(serviceUnit) {
			units = append(units, serviceUnit)
		}
		for _, h := range deviceHolders(trackerNodes(usbConf)) {
			switch {
			case h.Unit != "" && !slices.Contains(units, h.Unit):
				units = append(units, h.Unit)
			case h.Unit == "":
				log.Warn("the tracker is open in another program: close it, the telemetry will connect as soon as it is free",
					"pid", h.PID, "command", h.Comm)
			}
		}
		if len(units) == 0 && systemUnitActive(serviceUnit) {
			log.Warn("a system-wide " + serviceUnit + " is active and may hold the tracker: stop it with sudo systemctl stop tobii4c")
		}
	}
	var stoppedMu sync.Mutex
	var stopped []string
	stopUnits := func() error {
		for _, u := range units {
			log.Info("stopping systemd service", "unit", u)
			if err := systemctl("stop", u); err != nil {
				return err
			}
			stoppedMu.Lock()
			stopped = append(stopped, u)
			stoppedMu.Unlock()
		}
		return nil
	}
	var prompt *telemetry.Prompt
	if len(units) > 0 {
		if *stopService {
			if err := stopUnits(); err != nil {
				return err
			}
		} else {
			who := "Il servizio systemd " + units[0] + " sta usando"
			if len(units) > 1 {
				who = "I servizi systemd " + strings.Join(units, ", ") + " stanno usando"
			}
			lines := []string{
				who + " il tracker, che accetta un solo programma alla volta.",
				"Per la telemetria va fermato (systemctl --user stop). Mentre questa finestra è aperta, i programmi che ricevono i dati dal servizio (UDP, LSL, WebSocket) restano senza.",
			}
			if *restart {
				lines = append(lines, "Alla chiusura della finestra verrà riavviato.")
			} else {
				lines = append(lines, "Con -restart-service=false resterà fermo anche dopo la chiusura.")
			}
			prompt = &telemetry.Prompt{Title: "Il driver è già in esecuzione come servizio", Lines: lines,
				Confirm: "ferma il servizio e continua", Do: stopUnits}
		}
	}
	defer func() {
		stoppedMu.Lock()
		defer stoppedMu.Unlock()
		if !*restart {
			return
		}
		for i := len(stopped) - 1; i >= 0; i-- {
			if err := systemctl("start", stopped[i]); err != nil {
				fmt.Fprintln(os.Stderr, "riavvio del servizio non riuscito:", err)
			} else {
				fmt.Fprintln(os.Stderr, "servizio", stopped[i], "riavviato")
			}
		}
	}()

	// stimatore del viso: produce l'anteprima con i landmark
	est, err := hp.setup(log)
	if err != nil {
		return err
	}
	if est != nil {
		defer est.Close()
		col.HP = est
	}

	var sinks []tracker.Sink
	sinks = append(sinks, col)
	if *udpAddr != "" {
		u, err := sink.NewUDP(*udpAddr)
		if err != nil {
			return fmt.Errorf("udp: %w", err)
		}
		defer u.Close()
		sinks = append(sinks, u)
		log.Info("udp output", "addr", *udpAddr)
	}

	// finestra
	conn, err := x11ui.Connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	mons, err := x11ui.Monitors(conn)
	if err != nil {
		return err
	}
	mon := mons[0]
	for _, m := range mons {
		if (*monName == "" && m.Primary) || (*monName != "" && strings.EqualFold(m.Name, *monName)) {
			mon = m
		}
	}
	if *monName != "" && !strings.EqualFold(mon.Name, *monName) {
		return fmt.Errorf("monitor %q non trovato (usa tobii4c calibrate -monitors)", *monName)
	}
	w, h := mon.W, mon.H
	if *size != "" {
		var err error
		var fw, fh float64
		if fw, fh, err = parseSize(*size); err != nil {
			return fmt.Errorf("-size: %w", err)
		}
		w, h = int(fw), int(fh)
	}
	win, err := x11ui.OpenWindow(conn, x11ui.WindowOptions{Title: "tobii4c · telemetria", Class: "tobii4c-telemetry",
		X: mon.X, Y: mon.Y, W: w, H: h, MinW: 960, MinH: 540, Background: 0x45494f, Maximized: *maximized, Fullscreen: *fullscreen})
	if err != nil {
		return err
	}
	defer win.Close()

	ctx, cancel := signalContext()
	defer cancel()
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	trackerCtx, stopTracker := context.WithCancel(context.Background())
	defer stopTracker()
	trackerDone := make(chan error, 1)

	app := telemetry.NewApp(win, col, log)
	app.ShotDir = *shots
	app.AutoShot = *autoShot
	app.Prompt = prompt
	if *simulate {
		app.Title = "simulazione"
		img, err := simulationImage(*simImage)
		if err != nil {
			return err
		}
		app.Start = func() {
			go func() {
				telemetry.Simulate(trackerCtx, col, est, img)
				trackerDone <- nil
			}()
		}
	} else {
		opts := tracker.Options{
			USB: usbConf, KeyMap: km, ExtraStreams: extraStreams, Presence: true,
			RequestTimeout: c.reqTimeout, KeepAlive: *keepAlive, StatsInterval: *stats,
			Hook: col.Hook, HookGaze: true, OnReady: col.OnReady, OnClose: col.OnClose, OnNotification: col.OnNotification,
			ImagesWhenAway: hp.always,
		}
		if est != nil {
			opts.HeadPose = est
		}
		t := tracker.New(opts, log, sinks...)
		col.Stats = &t.Stats
		app.Start = func() {
			go func() {
				err := t.Run(trackerCtx)
				if err != nil {
					log.Error("tracker stopped", "err", err)
				}
				trackerDone <- err
			}()
		}
	}
	log.Info("telemetry window open", "monitor", mon.Name, "size", fmt.Sprintf("%dx%d", w, h))
	runErr := app.Run(ctx)
	win.Close()
	stopTracker()
	if app.Started() {
		select {
		case err := <-trackerDone:
			if err != nil && runErr == nil && !errors.Is(err, context.Canceled) {
				runErr = err
			}
		case <-time.After(5 * time.Second):
			log.Warn("tracker did not stop within 5 s")
		}
	}
	return runErr
}

// simulationImage carica l'immagine per -simulate-image (nil se vuoto).
func simulationImage(path string) (*irimg.Frame, error) {
	if path == "" {
		return nil, nil
	}
	return loadGrayImage(path)
}
