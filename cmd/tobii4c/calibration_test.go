package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/store"
	"github.com/ablescia/tobii4c-linux/internal/tracker"
)

// configHome sposta ~/.config in una cartella temporanea (os.UserConfigDir
// legge XDG_CONFIG_HOME) e restituisce un logger che scrive in buf.
func configHome(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// Con "saved" e nessun file salvato il servizio deve partire lo stesso:
// niente errore, nessuna scrittura sul dispositivo, un avviso nel log.
func TestSavedArgsWithoutFiles(t *testing.T) {
	log, buf := configHome(t)
	blob, err := loadCalibrationArg("saved", log)
	if err != nil || blob != nil {
		t.Fatalf("calibrazione: %v, %d byte", err, len(blob))
	}
	disp, err := loadDisplayArg("saved", 0, log)
	if err != nil || disp != nil {
		t.Fatalf("display: %v, %+v", err, disp)
	}
	for _, want := range []string{"no saved calibration", "no saved display geometry"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("manca l'avviso %q nel log:\n%s", want, buf)
		}
	}
}

func TestSavedArgsWithFiles(t *testing.T) {
	log, _ := configHome(t)
	if err := store.SaveDisplay(store.Display{WidthMM: 698, HeightMM: 393, OffsetXMM: -1.5}); err != nil {
		t.Fatal(err)
	}
	disp, err := loadDisplayArg("saved", 99, log) // -display-offset-x è ignorato con "saved"
	if err != nil {
		t.Fatal(err)
	}
	if want := (tracker.DisplaySpec{WidthMM: 698, HeightMM: 393, OffsetXMM: -1.5}); disp == nil || *disp != want {
		t.Fatalf("display %+v, atteso %+v", disp, want)
	}

	p, err := store.CalibrationPath()
	if err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte{0xab}, minPlausibleBlob)
	if err := os.WriteFile(p, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadCalibrationArg("saved", log)
	if err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("calibrazione: %v, %d byte", err, len(got))
	}
}

// Un file indicato esplicitamente, o salvato ma non valido, resta un errore.
func TestExplicitArgsErrors(t *testing.T) {
	log, _ := configHome(t)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := loadCalibrationArg(filepath.Join(t.TempDir(), "manca.bin"), log); err == nil {
		t.Error("calibrazione mancante indicata a mano: nessun errore")
	}
	small := filepath.Join(t.TempDir(), "piccolo.bin")
	if err := os.WriteFile(small, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCalibrationArg(small, discard); err == nil {
		t.Error("blob troppo piccolo accettato")
	}
	if _, err := loadDisplayArg("698", 0, discard); err == nil {
		t.Error("-display-mm senza altezza accettato")
	}
	if err := store.SaveDisplay(store.Display{}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDisplayArg("saved", 0, discard); err == nil {
		t.Error("display.json con dimensioni nulle accettato")
	}
	disp, err := loadDisplayArg("698x393", 12, discard)
	if err != nil || disp == nil || *disp != (tracker.DisplaySpec{WidthMM: 698, HeightMM: 393, OffsetXMM: 12}) {
		t.Errorf("-display-mm 698x393: %+v, %v", disp, err)
	}
}
