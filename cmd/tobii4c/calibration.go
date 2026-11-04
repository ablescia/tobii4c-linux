package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/store"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// minPlausibleBlob: un blob reale pesa centinaia di KB; sotto questa soglia è
// quasi certamente un file sbagliato.
const minPlausibleBlob = 4096

func parseSize(s string) (w, h float64, err error) {
	if _, err = fmt.Sscanf(strings.ToLower(s), "%gx%g", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, errors.New("formato atteso LARGHEZZAxALTEZZA in mm, es. 698x393")
	}
	return w, h, nil
}

func loadCalibrationArg(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	if path == "saved" {
		p, err := store.CalibrationPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("calibrazione: %w", err)
	}
	if len(b) < minPlausibleBlob {
		return nil, fmt.Errorf("calibrazione %s: %d byte, troppo piccolo per un blob reale", path, len(b))
	}
	return b, nil
}

// oneShot apre una sessione senza streaming, esegue fn e chiude.
func oneShot(c *common, fn func(s *tracker.Session) error) error {
	log := c.logger()
	usbConf, err := c.usb()
	if err != nil {
		return err
	}
	var fnErr error
	ran := false
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancelT := context.WithTimeout(ctx, 90*time.Second)
	defer cancelT()
	opt := tracker.Options{USB: usbConf, RequestTimeout: c.reqTimeout, NoSubscribe: true, FailFast: true,
		OnReady: func(s *tracker.Session) error { ran = true; fnErr = fn(s); return nil }}
	if err := tracker.New(opt, log).Run(ctx); err != nil {
		return err
	}
	if !ran {
		return errors.New("nessuna sessione col dispositivo")
	}
	return fnErr
}

func cmdCalibration(args []string) error {
	if len(args) == 0 {
		return errors.New("uso: tobii4c calibration save|apply|restore [flag]")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("calibration "+sub, flag.ExitOnError)
	var c common
	c.register(fs)
	file := fs.String("f", "", "file del blob (default ~/.config/tobii4c/calibration.bin)")
	fs.Parse(args)
	path := *file
	if path == "" {
		p, err := store.CalibrationPath()
		if err != nil {
			return err
		}
		path = p
	}
	switch sub {
	case "save":
		return oneShot(&c, func(s *tracker.Session) error {
			t0 := time.Now()
			blob, err := s.ReadCalibration()
			if err != nil {
				return fmt.Errorf("lettura calibrazione: %w", err)
			}
			if err := store.WriteAtomic(path, blob); err != nil {
				return err
			}
			fmt.Printf("calibrazione salvata: %s (%d byte, letta in %s)\n", path, len(blob), time.Since(t0).Round(time.Millisecond))
			return nil
		})
	case "apply", "restore":
		if sub == "restore" && *file == "" {
			b, err := store.Backups()
			if err != nil || len(b) == 0 {
				return errors.New("nessun backup in ~/.config/tobii4c/backups")
			}
			path = b[0]
		}
		blob, err := loadCalibrationArg(path)
		if err != nil {
			return err
		}
		return oneShot(&c, func(s *tracker.Session) error {
			if old, err := s.ReadCalibration(); err == nil {
				if p, err := store.SaveBackup(old, "before-apply"); err == nil {
					fmt.Println("backup della calibrazione precedente:", p)
				}
			}
			t0 := time.Now()
			if err := s.WriteCalibration(blob); err != nil {
				return fmt.Errorf("scrittura calibrazione: %w", err)
			}
			fmt.Printf("calibrazione applicata: %s (%d byte, in %s)\n", path, len(blob), time.Since(t0).Round(time.Millisecond))
			return nil
		})
	}
	return fmt.Errorf("sottocomando sconosciuto %q (save|apply|restore)", sub)
}

func cmdDisplay(args []string) error {
	fs := flag.NewFlagSet("display", flag.ExitOnError)
	var c common
	c.register(fs)
	set := fs.String("set", "", "imposta: LARGHEZZAxALTEZZA in mm dello schermo (area attiva)")
	offX := fs.Float64("offset-x", 0, "mm di cui il centro dello schermo sta a destra del centro del tracker")
	fs.Parse(args)
	return oneShot(&c, func(s *tracker.Session) error {
		d, m := s.DisplayInfo()
		if m != nil {
			fmt.Printf("mounting     %d segni guida a %.1f mm, camera inclinata di %.1f°, external %.2f, internal %.2f\n",
				m.Guides, m.WidthMM, m.AngleDeg, m.ExternalMM, m.InternalMM)
		}
		if d != nil {
			fmt.Printf("display area %s, centro a %+.1f mm dal tracker\n", d, d.OffsetX())
		}
		if *set == "" {
			return nil
		}
		if m == nil {
			return errors.New("il dispositivo non ha restituito il mounting: impossibile calcolare la display area")
		}
		w, h, err := parseSize(*set)
		if err != nil {
			return err
		}
		area := ttp.DisplayAreaBasic(w, h, *offX, *m)
		if err := s.WriteDisplayArea(area); err != nil {
			return err
		}
		nd, _ := s.DisplayInfo()
		fmt.Printf("nuova area   %s\n", nd)
		return store.SaveDisplay(store.Display{WidthMM: w, HeightMM: h, OffsetXMM: *offX, Area: nd})
	})
}
