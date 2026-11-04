//go:build !lsl

package sink

import (
	"errors"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

// LSLAvailable indica se il binario è stato compilato con -tags lsl.
const LSLAvailable = false

// LSL è un segnaposto: ricompilare con `go build -tags lsl` (richiede liblsl).
type LSL struct{}

// NewLSL fallisce sempre nei build senza tag lsl.
func NewLSL(name, sourceID string, nominalHz float64) (*LSL, error) {
	return nil, errors.New("supporto LSL non compilato: ricompila con `go build -tags lsl` (serve liblsl)")
}

// Write non fa nulla.
func (*LSL) Write(*gaze.Sample) error { return nil }

// Close non fa nulla.
func (*LSL) Close() error { return nil }

// LSLReceive fallisce sempre nei build senza tag lsl.
func LSLReceive(name string, dur time.Duration) (*LSLReport, error) {
	return nil, errors.New("supporto LSL non compilato: ricompila con `go build -tags lsl` (serve liblsl)")
}
