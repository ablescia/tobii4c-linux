package sink

import (
	"errors"
	"fmt"
	"io"
	"math"
	"syscall"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

func isConnRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

// Print scrive una riga leggibile al massimo ogni Every (diagnostica).
type Print struct {
	W     io.Writer
	Every time.Duration
	last  time.Time
}

// Write stampa il campione se è passato abbastanza tempo dall'ultimo.
func (p *Print) Write(s *gaze.Sample) error {
	now := time.Now()
	if now.Sub(p.last) < p.Every {
		return nil
	}
	p.last = now
	_, err := fmt.Fprintln(p.W, Format(s))
	return err
}

// Close non fa nulla.
func (p *Print) Close() error { return nil }

// Format rende un campione su una riga.
func Format(s *gaze.Sample) string {
	v := func(b bool) string {
		if b {
			return "ok"
		}
		return "--"
	}
	deg := func(r float32) float64 { return float64(r) * 180 / math.Pi }
	open := func(e *gaze.Eye) string {
		if !e.OpennessValid {
			return "--"
		}
		return fmt.Sprintf("%.1f", e.OpennessMM)
	}
	pitch := "--"
	if s.Head.RotationValid[0] {
		pitch = fmt.Sprintf("%+.1f°", deg(s.Head.RotationRad[0]))
	}
	return fmt.Sprintf("#%-7d dev=%dus gaze=(%.3f,%.3f)%s  L:%s R:%s  head=(%.0f,%.0f,%.0f)mm%s pitch=%s yaw=%+.1f° roll=%+.1f°  pupil=%.2f/%.2fmm  open=%s/%smm  %s",
		s.Seq, s.DeviceTimeUs, s.Gaze[0], s.Gaze[1], v(s.GazeValid),
		v(s.Left.Tracked), v(s.Right.Tracked),
		s.Head.PositionMM[0], s.Head.PositionMM[1], s.Head.PositionMM[2], v(s.Head.PositionValid),
		pitch, deg(s.Head.RotationRad[1]), deg(s.Head.RotationRad[2]),
		s.Left.PupilMM, s.Right.PupilMM, open(&s.Left), open(&s.Right), s.Presence)
}
