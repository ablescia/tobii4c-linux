package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/calib"
	"github.com/ablescia/tobii4c-linux/internal/calibui"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

func cmdCalibrate(args []string) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	var c common
	c.register(fs)
	monName := fs.String("monitor", "", "monitor su cui calibrare (default: il primario); vedi -monitors")
	listMon := fs.Bool("monitors", false, "elenca i monitor ed esce")
	simulate := fs.Bool("simulate", false, "nessun tracker: occhi e sguardo simulati (prova dell'interfaccia)")
	radius := fs.Float64("focus-radius-mm", calib.DefaultConfig().FocusRadiusMM, "raggio entro cui lo sguardo deve cadere sul punto")
	fs.Parse(args)
	log := c.logger()

	conn, err := x11ui.Connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	mons, err := x11ui.Monitors(conn)
	if err != nil {
		return err
	}
	if *listMon {
		for _, m := range mons {
			fmt.Printf("%-10s %dx%d+%d+%d  %.0f×%.0f mm  %s%s\n", m.Name, m.W, m.H, m.X, m.Y, m.WidthMM, m.HeightMM, m.Model,
				map[bool]string{true: "  (primario)"}[m.Primary])
		}
		return nil
	}
	mon := mons[0]
	for _, m := range mons {
		if (*monName == "" && m.Primary) || (*monName != "" && strings.EqualFold(m.Name, *monName)) {
			mon = m
		}
	}
	if *monName != "" && !strings.EqualFold(mon.Name, *monName) {
		return fmt.Errorf("monitor %q non trovato (usa -monitors)", *monName)
	}

	cfg := calib.DefaultConfig()
	cfg.FocusRadiusMM = *radius
	ctrl := calib.New(cfg, log)
	ctx, cancel := signalContext()
	defer cancel()

	// Il tracker gira su un contesto proprio: all'uscita la calibrazione
	// eventualmente in corso viene annullata PRIMA di chiudere la sessione USB.
	trackerCtx, stopTracker := context.WithCancel(context.Background())
	defer stopTracker()
	trackerDone := make(chan error, 1)
	if *simulate {
		sim := calib.NewSimDevice(ctrl.Hub)
		sim.OnNotify = ctrl.Notify
		ctrl.SetDevice(sim)
		go sim.Run(trackerCtx)
		close(trackerDone)
	} else {
		usbConf, err := c.usb()
		if err != nil {
			return err
		}
		opt := tracker.Options{USB: usbConf, RequestTimeout: c.reqTimeout}
		ctrl.TrackerHooks(&opt)
		t := tracker.New(opt, log, ctrl.Hub)
		go func() { trackerDone <- t.Run(trackerCtx) }()
		// Attende la prima connessione prima di prendere lo schermo.
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
	log.Info("calibration window open", "monitor", mon.Name, "size", fmt.Sprintf("%dx%d", mon.W, mon.H),
		"mm", fmt.Sprintf("%.0fx%.0f", mon.WidthMM, mon.HeightMM), "model", mon.Model)
	runErr := app.Run(ctx)
	win.Close()
	ctrl.AbortIfActive()
	stopTracker()
	if err := <-trackerDone; err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}
