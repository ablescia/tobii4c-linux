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
	fs.Parse(args)
	log := c.logger()

	camPath, err := store.CameraPath()
	if err != nil {
		return err
	}
	userPath, err := store.HeadPoseUserPath()
	if err != nil {
		return err
	}

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

	est, err := headpose.New(headpose.Config{CameraPath: camPath, UserPath: userPath,
		MaxRate: 30, ManualCalibration: true}, log)
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
	runErr := app.Run(ctx)
	win.Close()
	stopTracker()
	if err := <-trackerDone; err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}
