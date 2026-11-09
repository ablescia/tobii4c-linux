//go:build linux

package vk

import (
	"bytes"
	"testing"
)

func open(t *testing.T) *Device {
	t.Helper()
	d, err := Open()
	if err != nil {
		t.Skipf("Vulkan non disponibile: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}

// TestCopyRoundTrip: dati dalla CPU alla memoria della GPU e ritorno, con
// un riempimento e i timestamp.
func TestCopyRoundTrip(t *testing.T) {
	d := open(t)
	t.Logf("%+v", d.Info)
	if d.Info.Name == "" || d.Info.MaxInvocations < 128 || d.Info.SharedMem < 16384 {
		t.Fatalf("proprietà del device non lette: %+v", d.Info)
	}
	const n = 1 << 16
	dev, err := d.NewBuffer(n, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Destroy()
	host, err := d.NewBuffer(n, true)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Destroy()
	src := make([]byte, n)
	for i := range src {
		src[i] = byte(i*31 + i>>8)
	}
	if err := d.Upload(dev, 0, src); err != nil {
		t.Fatal(err)
	}
	p, err := d.Record(2, func(r *Recorder) {
		r.Timestamp(0)
		r.Copy(dev, 0, host, 0, n)
		r.Fill(host, n-64, 64, 0x01020304)
		r.Timestamp(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Destroy()
	for i := 0; i < 3; i++ { // il programma si riesegue
		clear(host.Bytes())
		if err := p.Run(); err != nil {
			t.Fatal(err)
		}
		got := host.Bytes()[:n]
		if !bytes.Equal(got[:n-64], src[:n-64]) {
			t.Fatal("copia diversa dall'originale")
		}
		if got[n-64] != 4 || got[n-61] != 1 {
			t.Fatalf("riempimento: % x", got[n-64:n-60])
		}
	}
	if p.HasTimestamps() {
		var ts [2]float64
		if err := p.Timestamps(ts[:]); err != nil {
			t.Fatal(err)
		}
		if ts[1] <= 0 || ts[1] > 1e8 {
			t.Errorf("timestamp: %v ns", ts[1])
		}
	}
	if _, err := d.NewPipeline([]byte("non è SPIR-V")); err == nil {
		t.Error("SPIR-V non valido accettato")
	}
}
