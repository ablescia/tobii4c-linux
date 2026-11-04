package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/calib"
	"github.com/ablescia/tobii4c-linux/internal/calibui"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/store"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// cmdHeadPoseCalibrate apre l'applicazione guidata per il pitch della testa:
// calibrazione della camera del 4C, zero personale del viso, verifica.
func cmdHeadPoseCalibrate(args []string) error {
	fs := flag.NewFlagSet("headpose-calibrate", flag.ExitOnError)
	var c common
	c.register(fs)
	monName := fs.String("monitor", "", "monitor da usare (default: il primario); vedi tobii4c calibrate -monitors")
	model := fs.String("model", defaultModelPath(), "modello ONNX dei landmark del viso (make headpose-model)")
	sessions := fs.String("sessions", "", "cartella delle registrazioni (default ~/.cache/tobii4c/headpose)")
	shots := fs.Bool("screenshots", false, "salva anche la finestra come PNG nella cartella della sessione")
	from := fs.String("from", "", "senza interfaccia: ricalcola la calibrazione della camera da sessioni registrate (cartelle separate da virgole) e la salva")
	serial := fs.String("serial", "", "con -from: numero di serie del 4C (default: quello registrato nella sessione)")
	fs.Parse(args)
	log := c.logger()
	if *from != "" {
		return calibrateFromSessions(strings.Split(*from, ","), *serial)
	}

	if _, err := os.Stat(*model); err != nil {
		return fmt.Errorf("modello del viso: %w (installalo con make headpose-model)", err)
	}
	camPath, err := store.CameraPath()
	if err != nil {
		return err
	}
	userPath, err := store.HeadPoseUserPath()
	if err != nil {
		return err
	}
	base := *sessions
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		base = filepath.Join(cache, "tobii4c", "headpose")
	}
	dir := filepath.Join(base, time.Now().Format("20060102-150405"))
	imgDir := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return err
	}
	log.Info("head pose session", "dir", dir)

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
		return fmt.Errorf("monitor %q non trovato", *monName)
	}

	est, err := headpose.New(headpose.Config{ModelPath: *model, CameraPath: camPath, UserPath: userPath,
		MaxRate: 30, ManualCalibration: true, RecordDir: dir, DebugDir: imgDir}, log)
	if err != nil {
		return err
	}
	defer est.Close()

	ctrl := calib.New(calib.DefaultConfig(), log)
	ctx, cancel := signalContext()
	defer cancel()
	trackerCtx, stopTracker := context.WithCancel(context.Background())
	defer stopTracker()
	usbConf, err := c.usb()
	if err != nil {
		return err
	}
	opt := tracker.Options{USB: usbConf, RequestTimeout: c.reqTimeout, Presence: true, HeadPose: est}
	ctrl.TrackerHooks(&opt)
	t := tracker.New(opt, log, ctrl.Hub)
	trackerDone := make(chan error, 1)
	go func() { trackerDone <- t.Run(trackerCtx) }()
	deadline := time.After(8 * time.Second)
wait:
	for {
		if _, err := ctrl.Device(); err == nil {
			break
		}
		select {
		case err := <-trackerDone:
			if err == nil {
				err = errors.New("tracker terminato")
			}
			return err
		case <-deadline:
			log.Warn("tracker non ancora connesso: apro comunque l'interfaccia")
			break wait
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}

	win, err := x11ui.Open(conn, mon, 0x45494f)
	if err != nil {
		return err
	}
	app, err := calibui.New(win, ctrl, log)
	if err != nil {
		win.Close()
		return err
	}
	app.HP = est
	app.Start = calibui.HeadPoseStart
	if *shots {
		app.ShotDir = dir
	}
	runErr := app.Run(ctx)
	win.Close()
	stopTracker()
	if err := <-trackerDone; err != nil && runErr == nil {
		runErr = err
	}
	fmt.Fprintln(os.Stderr, "dati della sessione:", dir)
	return runErr
}

// calibrateFromSessions ricalcola la calibrazione della camera dalle
// registrazioni di headpose-calibrate e la salva se è affidabile.
func calibrateFromSessions(dirs []string, serial string) error {
	if serial == "" {
		var meta struct {
			Serial string `json:"serial"`
		}
		if data, err := os.ReadFile(filepath.Join(dirs[0], "session.json")); err == nil {
			_ = json.Unmarshal(data, &meta)
		}
		serial = meta.Serial
	}
	if serial == "" {
		return errors.New("numero di serie sconosciuto: usa -serial (tobii4c info lo mostra)")
	}
	samples, err := headpose.RecordingSamples(dirs...)
	if err != nil {
		return err
	}
	fmt.Printf("%d campioni da %d sessioni, calcolo…\n", len(samples), len(dirs))
	cam, q, err := headpose.CalibrateSamples(samples, 640, 480)
	if err != nil {
		return err
	}
	fmt.Printf("errore di riproiezione %.2f px, incertezza della rotazione %.2f°\n", q.RMSPx, q.SigmaDeg)
	fmt.Printf("focale %.0f px, punto principale %.0f,%.0f, camera nel tracker %.0f,%.0f,%.0f mm, inclinazione %.1f°, speculare %v\n",
		cam.F, cam.CX, cam.CY, cam.C[0], cam.C[1], cam.C[2], headpose.CameraTiltDeg(cam), cam.Mirror)
	if !headpose.CalibrationReliable(q) {
		return errors.New("calibrazione non abbastanza precisa: non salvata")
	}
	path, err := store.CameraPath()
	if err != nil {
		return err
	}
	if err := cam.Save(path, serial, q); err != nil {
		return err
	}
	fmt.Println("salvata in", path, "per il 4C", serial)
	return nil
}
