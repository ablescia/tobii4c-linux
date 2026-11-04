package sink

import (
	"math"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

// lslChannels: ordine e nomi dei canali dell'outlet LSL (float32). I primi 12
// sono quelli della prima versione e hanno validità esplicita (gaze_valid,
// head_valid per la posizione); gli angoli della testa e i canali successivi
// valgono NaN quando il dato non è valido. Yaw e roll possono essere validi
// con head_valid a 0: vengono dal viso nelle immagini IR anche quando il
// tracker non vede tutti e due gli occhi.
var lslChannels = [lslChannelCount]string{
	"gaze_x", "gaze_y", "gaze_valid",
	"head_x_mm", "head_y_mm", "head_z_mm", "head_pitch_rad", "head_yaw_rad", "head_roll_rad", "head_valid",
	"pupil_left_mm", "pupil_right_mm",
	"presence",
	"origin_left_x_mm", "origin_left_y_mm", "origin_left_z_mm",
	"origin_right_x_mm", "origin_right_y_mm", "origin_right_z_mm",
	"trackbox_left_x", "trackbox_left_y", "trackbox_left_z",
	"trackbox_right_x", "trackbox_right_y", "trackbox_right_z",
	"openness_left_mm", "openness_right_mm",
}

const lslChannelCount = 27

// lslFill scrive in v i valori dei canali di s, nell'ordine di lslChannels.
func lslFill(v *[lslChannelCount]float32, s *gaze.Sample) {
	b := func(ok bool) float32 {
		if ok {
			return 1
		}
		return 0
	}
	nan := float32(math.NaN())
	or := func(x float32, ok bool) float32 {
		if ok {
			return x
		}
		return nan
	}
	h := s.Head
	l, r := &s.Left, &s.Right
	*v = [lslChannelCount]float32{
		s.Gaze[0], s.Gaze[1], b(s.GazeValid),
		h.PositionMM[0], h.PositionMM[1], h.PositionMM[2],
		or(h.RotationRad[0], h.RotationValid[0]), or(h.RotationRad[1], h.RotationValid[1]), or(h.RotationRad[2], h.RotationValid[2]), b(h.PositionValid),
		l.PupilMM, r.PupilMM,
		float32(s.Presence),
		or(l.OriginDisplayMM[0], l.OriginDisplayValid), or(l.OriginDisplayMM[1], l.OriginDisplayValid), or(l.OriginDisplayMM[2], l.OriginDisplayValid),
		or(r.OriginDisplayMM[0], r.OriginDisplayValid), or(r.OriginDisplayMM[1], r.OriginDisplayValid), or(r.OriginDisplayMM[2], r.OriginDisplayValid),
		or(l.TrackBox[0], l.TrackBoxValid), or(l.TrackBox[1], l.TrackBoxValid), or(l.TrackBox[2], l.TrackBoxValid),
		or(r.TrackBox[0], r.TrackBoxValid), or(r.TrackBox[1], r.TrackBoxValid), or(r.TrackBox[2], r.TrackBoxValid),
		or(l.OpennessMM, l.OpennessValid), or(r.OpennessMM, r.OpennessValid),
	}
}
