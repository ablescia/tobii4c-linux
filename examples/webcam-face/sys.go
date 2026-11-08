//go:build linux

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sysStats sono le risorse usate dal processo, aggiornate una volta al
// secondo.
type sysStats struct {
	at        time.Time
	cpu       time.Duration // utente + sistema, cumulativo
	CPUPct    float64       // % di un core nell'ultimo intervallo
	RSS       uint64        // byte
	HeapAlloc uint64
	HeapSys   uint64
	TotalSys  uint64
	NumGC     uint32
	LastPause time.Duration
	PauseTot  time.Duration
	GCPerSec  float64
	prevGC    uint32
	Gorout    int
	Threads   int
	Started   time.Time
}

func newSysStats() sysStats { return sysStats{Started: time.Now()} }

func (s *sysStats) update(now time.Time) {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
		if !s.at.IsZero() {
			if dt := now.Sub(s.at); dt > 0 {
				s.CPUPct = 100 * float64(cpu-s.cpu) / float64(dt)
			}
		}
		s.cpu = cpu
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.HeapAlloc, s.HeapSys, s.TotalSys = ms.HeapAlloc, ms.HeapSys, ms.Sys
	if !s.at.IsZero() {
		if dt := now.Sub(s.at).Seconds(); dt > 0 {
			s.GCPerSec = float64(ms.NumGC-s.prevGC) / dt
		}
	}
	s.prevGC = ms.NumGC
	s.NumGC = ms.NumGC
	s.PauseTot = time.Duration(ms.PauseTotalNs)
	if ms.NumGC > 0 {
		s.LastPause = time.Duration(ms.PauseNs[(ms.NumGC+255)%256])
	}
	s.Gorout = runtime.NumGoroutine()
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			switch {
			case strings.HasPrefix(l, "VmRSS:"):
				f := strings.Fields(l)
				if len(f) >= 2 {
					kb, _ := strconv.ParseUint(f[1], 10, 64)
					s.RSS = kb * 1024
				}
			case strings.HasPrefix(l, "Threads:"):
				f := strings.Fields(l)
				if len(f) >= 2 {
					s.Threads, _ = strconv.Atoi(f[1])
				}
			}
		}
	}
	s.at = now
}
