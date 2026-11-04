//go:build lsl

package sink

/*
#cgo LDFLAGS: -llsl
#include <stdlib.h>
#include <lsl_c.h>
*/
import "C"

import (
	"errors"
	"time"
	"unsafe"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

// LSLAvailable indica se il binario è stato compilato con -tags lsl.
const LSLAvailable = true

// LSL pubblica un outlet "Gaze" a lslChannelCount canali float32 (vedi
// lslChannels in lsl_channels.go).
type LSL struct {
	info   C.lsl_streaminfo
	outlet C.lsl_outlet
	buf    [lslChannelCount]float32 // C.float è float32: passato a liblsl così com'è
}

// NewLSL crea l'outlet. nominalHz è solo informativo (90 per il 4C).
func NewLSL(name, sourceID string, nominalHz float64) (*LSL, error) {
	cname, ctype, csrc := C.CString(name), C.CString("Gaze"), C.CString(sourceID)
	defer C.free(unsafe.Pointer(cname))
	defer C.free(unsafe.Pointer(ctype))
	defer C.free(unsafe.Pointer(csrc))
	info := C.lsl_create_streaminfo(cname, ctype, C.int32_t(lslChannelCount), C.double(nominalHz), C.cft_float32, csrc)
	if info == nil {
		return nil, errors.New("lsl_create_streaminfo failed")
	}
	desc := C.lsl_get_desc(info)
	chans := appendChild(desc, "channels")
	for _, ch := range lslChannels {
		c := appendChild(chans, "channel")
		appendValue(c, "label", ch)
	}
	outlet := C.lsl_create_outlet(info, 0, 2)
	if outlet == nil {
		C.lsl_destroy_streaminfo(info)
		return nil, errors.New("lsl_create_outlet failed")
	}
	return &LSL{info: info, outlet: outlet}, nil
}

func appendChild(e C.lsl_xml_ptr, name string) C.lsl_xml_ptr {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	return C.lsl_append_child(e, cs)
}

func appendValue(e C.lsl_xml_ptr, name, value string) {
	cn, cv := C.CString(name), C.CString(value)
	defer C.free(unsafe.Pointer(cn))
	defer C.free(unsafe.Pointer(cv))
	C.lsl_append_child_value(e, cn, cv)
}

// Write pubblica il campione con timestamp LSL retrodatato all'istante di
// ricezione USB (lsl_local_clock − latenza della pipeline).
func (l *LSL) Write(s *gaze.Sample) error {
	lslFill(&l.buf, s)
	age := time.Since(time.Unix(0, s.HostTimeNs)).Seconds()
	ts := float64(C.lsl_local_clock()) - age
	if rc := C.lsl_push_sample_ft(l.outlet, (*C.float)(unsafe.Pointer(&l.buf[0])), C.double(ts)); rc < 0 {
		return errors.New("lsl_push_sample_ft failed")
	}
	return nil
}

// Close distrugge l'outlet.
func (l *LSL) Close() error {
	C.lsl_destroy_outlet(l.outlet)
	return nil
}
