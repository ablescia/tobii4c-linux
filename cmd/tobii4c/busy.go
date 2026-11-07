package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/usbdev"
)

// Chi tiene occupato il tracker: il driver vuole l'accesso esclusivo alla
// USB, e un altro programma che lo usa (il servizio tobii4c, o un servizio
// che avvia `tobii4c run`, come tobii-ws) lo rende inutilizzabile.

// deviceHolder è un processo che ha aperto il nodo USB del tracker.
type deviceHolder struct {
	PID  int
	Comm string
	Unit string // unit utente di systemd che lo contiene, "" se non è un servizio
}

// trackerNodes restituisce i nodi /dev/bus/usb/BBB/DDD dei tracker collegati.
func trackerNodes(conf usbdev.Config) []string {
	vendors, _ := filepath.Glob("/sys/bus/usb/devices/*/idVendor")
	var out []string
	read := func(dir, name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	for _, v := range vendors {
		dir := filepath.Dir(v)
		vid, err1 := strconv.ParseUint(read(dir, "idVendor"), 16, 16)
		pid, err2 := strconv.ParseUint(read(dir, "idProduct"), 16, 16)
		if err1 != nil || err2 != nil || uint16(vid) != uint16(conf.VendorID) {
			continue
		}
		known := false
		for _, p := range conf.ProductIDs {
			known = known || uint16(p) == uint16(pid)
		}
		bus, err1 := strconv.Atoi(read(dir, "busnum"))
		dev, err2 := strconv.Atoi(read(dir, "devnum"))
		if known && err1 == nil && err2 == nil {
			out = append(out, fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev))
		}
	}
	return out
}

// deviceHolders cerca in /proc i processi che hanno aperto uno dei nodi.
// Vede solo i processi dell'utente (gli altri non si possono ispezionare).
func deviceHolders(nodes []string) []deviceHolder {
	if len(nodes) == 0 {
		return nil
	}
	procs, _ := filepath.Glob("/proc/[0-9]*")
	self := os.Getpid()
	var out []deviceHolder
	for _, p := range procs {
		pid, err := strconv.Atoi(filepath.Base(p))
		if err != nil || pid == self {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(p, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(p, "fd", fd.Name()))
			if err != nil || !slices.Contains(nodes, target) {
				continue
			}
			comm, _ := os.ReadFile(filepath.Join(p, "comm"))
			cg, _ := os.ReadFile(filepath.Join(p, "cgroup"))
			out = append(out, deviceHolder{PID: pid, Comm: strings.TrimSpace(string(comm)), Unit: userUnit(string(cg))})
			break
		}
	}
	return out
}

// userUnit estrae da /proc/PID/cgroup il servizio utente di systemd che
// contiene il processo, es. ".../user@1000.service/app.slice/tobii-ws.service".
func userUnit(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		_, path, ok := strings.Cut(line, "::")
		if !ok {
			continue
		}
		inUser := false
		for _, part := range strings.Split(path, "/") {
			if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
				inUser = true
				continue
			}
			if inUser && strings.HasSuffix(part, ".service") {
				return part
			}
		}
	}
	return ""
}

// unitActive dice se l'unit utente è attiva.
func unitActive(unit string) bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "is-active", "--quiet", unit).Run() == nil
}

// systemUnitActive dice se è attiva un'unit di sistema con quel nome.
func systemUnitActive(unit string) bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

// systemctl esegue systemctl --user args.
func systemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}
