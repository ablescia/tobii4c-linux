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
	"fmt"
	"math"
	"time"
	"unsafe"
)

// LSLReceive cerca un outlet con questo nome, ne riceve i campioni per dur e
// restituisce il riepilogo (ricevitore di prova: tobii4c lsl-recv).
func LSLReceive(name string, dur time.Duration) (*LSLReport, error) {
	prop, val := C.CString("name"), C.CString(name)
	defer C.free(unsafe.Pointer(prop))
	defer C.free(unsafe.Pointer(val))
	var infos [4]C.lsl_streaminfo
	n := C.lsl_resolve_byprop(&infos[0], 4, prop, val, 1, 5.0)
	if n <= 0 {
		return nil, fmt.Errorf("nessun outlet LSL %q trovato in 5 s", name)
	}
	for i := 1; i < int(n); i++ {
		C.lsl_destroy_streaminfo(infos[i])
	}
	inlet := C.lsl_create_inlet(infos[0], 360, 0, 1)
	if inlet == nil {
		return nil, errors.New("lsl_create_inlet failed")
	}
	defer C.lsl_destroy_inlet(inlet)
	var ec C.int32_t
	full := C.lsl_get_fullinfo(inlet, 5.0, &ec)
	if ec != 0 || full == nil {
		return nil, fmt.Errorf("lsl_get_fullinfo: errore %d", ec)
	}
	defer C.lsl_destroy_streaminfo(full)
	r := &LSLReport{
		Name:      C.GoString(C.lsl_get_name(full)),
		Type:      C.GoString(C.lsl_get_type(full)),
		Channels:  int(C.lsl_get_channel_count(full)),
		NominalHz: float64(C.lsl_get_nominal_srate(full)),
	}
	chName, labName := C.CString("channels"), C.CString("label")
	defer C.free(unsafe.Pointer(chName))
	defer C.free(unsafe.Pointer(labName))
	ch := C.lsl_first_child(C.lsl_child(C.lsl_get_desc(full), chName))
	for i := 0; i < r.Channels && C.lsl_empty(ch) == 0; i++ {
		r.Labels = append(r.Labels, C.GoString(C.lsl_child_value_n(ch, labName)))
		ch = C.lsl_next_sibling(ch)
	}
	C.lsl_open_stream(inlet, 5.0, &ec)
	if ec != 0 {
		return nil, fmt.Errorf("lsl_open_stream: errore %d", ec)
	}
	r.TimeCorrection = float64(C.lsl_time_correction(inlet, 5.0, &ec))

	buf := make([]float32, r.Channels)
	r.NaN = make([]int, r.Channels)
	var ages []float64
	var first, last float64
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		ts := float64(C.lsl_pull_sample_f(inlet, (*C.float)(unsafe.Pointer(&buf[0])), C.int32_t(r.Channels), 0.5, &ec))
		if ec != 0 {
			return nil, fmt.Errorf("lsl_pull_sample_f: errore %d", ec)
		}
		if ts == 0 {
			continue // timeout
		}
		now := float64(C.lsl_local_clock())
		if r.Samples == 0 {
			first = ts
		} else if ts-last > 0.05 {
			r.GapsOver50ms++
		}
		last = ts
		r.Samples++
		ages = append(ages, (now-(ts+r.TimeCorrection))*1000)
		for i, v := range buf {
			if math.IsNaN(float64(v)) {
				r.NaN[i]++
			}
		}
	}
	if r.Samples > 1 {
		r.MeasuredHz = float64(r.Samples-1) / (last - first)
	}
	r.Last = append([]float32(nil), buf...)
	if len(ages) > 0 {
		sortFloats(ages)
		r.MedianAgeMs = ages[len(ages)/2]
	}
	return r, nil
}

func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
