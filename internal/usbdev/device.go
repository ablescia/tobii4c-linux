// Package usbdev è il trasporto USB in user-space verso l'interfaccia vendor
// del Tobii 4C, costruito su google/gousb (libusb-1.0).
//
// Layout reale del 4C (lsusb -v -d 2104:0127):
//
//	interfaccia 0  classe 0xff vendor   EP 0x83 bulk IN, 0x04 bulk OUT, 0x05 bulk OUT  ← TTP
//	interfaccia 1  UVC Video Control    (nessun endpoint)
//	interfaccia 2  UVC Video Streaming  EP 0x82 bulk IN, 640x480 8 bit, formato vendor
//
// Tutto il traffico TTP passa per 0x05 (OUT) e 0x83 (IN); 0x04 non è usato
// da nessuna implementazione nota e qui non viene mai scritto.
package usbdev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/gousb"
)

// ID USB noti.
const (
	VendorTobii = gousb.ID(0x2104)
	ProductIS4  = gousb.ID(0x0127) // Tobii Eye Tracker 4C ("EyeChip", IS4)
	ProductET5  = gousb.ID(0x0313) // Tobii Eye Tracker 5 (IS5)
)

// Richieste vendor di sessione (bmRequestType = Vendor|Host→Device|Interface).
const (
	reqTypeVendorIfaceOut = 0x41
	reqSessionOpen        = 0x41
	reqSessionClose       = 0x42
)

// Config configura l'apertura del dispositivo.
type Config struct {
	VendorID     gousb.ID
	ProductIDs   []gousb.ID // il primo trovato vince
	Interface    int        // 0
	EndpointIn   int        // 3 (0x83)
	EndpointOut  int        // 5 (0x05)
	TransferSize int        // byte per bulk transfer IN (multiplo di 512)
	InFlight     int        // transfer IN tenute in coda nel kernel
	WriteTimeout time.Duration
}

// DefaultConfig è la configurazione per il 4C.
func DefaultConfig() Config {
	return Config{
		VendorID:     VendorTobii,
		ProductIDs:   []gousb.ID{ProductIS4, ProductET5},
		Interface:    0,
		EndpointIn:   3,
		EndpointOut:  5,
		TransferSize: 64 * 1024, // 16 × 64 KiB in coda: più di tre immagini IR da 300 KB
		InFlight:     16,
		WriteTimeout: 2 * time.Second,
	}
}

// Info descrive il dispositivo aperto.
type Info struct {
	VendorID, ProductID gousb.ID
	Bus, Address        int
	Manufacturer        string
	Product             string
	Serial              string
	Speed               string
}

// Device è una sessione USB aperta sull'interfaccia vendor.
type Device struct {
	Info Info

	ctx    *gousb.Context
	dev    *gousb.Device
	cfg    *gousb.Config
	intf   *gousb.Interface
	out    *gousb.OutEndpoint
	stream *gousb.ReadStream

	conf        Config
	log         *slog.Logger
	sessionOpen bool
}

// ErrNotFound: nessun tracker collegato.
var ErrNotFound = errors.New("usbdev: no Tobii tracker found")

// OpenError avvolge un fallimento durante l'apertura (prima dello streaming).
type OpenError struct{ Err error }

func (e *OpenError) Error() string { return e.Err.Error() }
func (e *OpenError) Unwrap() error { return e.Err }

// Open apre il primo tracker trovato, sgancia i driver kernel, reclama
// l'interfaccia vendor, apre la sessione TTP e avvia lo stream IN.
func Open(conf Config, log *slog.Logger) (_ *Device, err error) {
	d := &Device{conf: conf, log: log, ctx: gousb.NewContext()}
	defer func() {
		if err != nil {
			d.Close()
			if !errors.Is(err, ErrNotFound) {
				err = &OpenError{Err: err}
			}
		}
	}()

	for _, pid := range conf.ProductIDs {
		d.dev, err = d.ctx.OpenDeviceWithVIDPID(conf.VendorID, pid)
		if err != nil {
			return nil, fmt.Errorf("open %s:%s: %w (permessi? vedi udev/70-tobii-4c.rules)", conf.VendorID, pid, err)
		}
		if d.dev != nil {
			break
		}
	}
	if d.dev == nil {
		return nil, ErrNotFound
	}
	d.dev.ControlTimeout = time.Second
	d.Info = Info{
		VendorID: d.dev.Desc.Vendor, ProductID: d.dev.Desc.Product,
		Bus: d.dev.Desc.Bus, Address: d.dev.Desc.Address, Speed: d.dev.Desc.Speed.String(),
	}
	d.Info.Manufacturer, _ = d.dev.Manufacturer()
	d.Info.Product, _ = d.dev.Product()
	d.Info.Serial, _ = d.dev.SerialNumber()

	// DetachKernelDriver: gousb lo espone tramite SetAutoDetach, che abilita
	// libusb_set_auto_detach_kernel_driver e, in Config(), chiama
	// libusb_detach_kernel_driver su ogni interfaccia (ErrorNotFound = nessun
	// driver legato, ignorato). Al rilascio libusb ricollega il driver kernel
	// dell'interfaccia reclamata.
	if err = d.dev.SetAutoDetach(true); err != nil {
		return nil, fmt.Errorf("set auto-detach: %w", err)
	}
	cfgNum, err := d.dev.ActiveConfigNum()
	if err != nil {
		return nil, fmt.Errorf("active config: %w", err)
	}
	if d.cfg, err = d.dev.Config(cfgNum); err != nil {
		return nil, fmt.Errorf("config %d (detach kernel driver): %w", cfgNum, err)
	}
	if d.intf, err = d.cfg.Interface(conf.Interface, 0); err != nil {
		if isBusy(err) {
			return nil, fmt.Errorf("claim interface %d: %w — un altro processo tiene il tracker (il servizio: systemctl --user stop tobii4c; un'altra istanza, Talon, tobiiusbserviced?)", conf.Interface, err)
		}
		return nil, fmt.Errorf("claim interface %d: %w", conf.Interface, err)
	}
	if d.out, err = d.intf.OutEndpoint(conf.EndpointOut); err != nil {
		return nil, fmt.Errorf("out endpoint 0x%02x: %w", conf.EndpointOut, err)
	}
	in, err := d.intf.InEndpoint(conf.EndpointIn)
	if err != nil {
		return nil, fmt.Errorf("in endpoint 0x%02x: %w", 0x80|conf.EndpointIn, err)
	}

	// Apertura sessione: control transfer vendor 0x41 a lunghezza zero, prima
	// di qualsiasi bulk (Tropaion/Tobii_Linux, Aetherall/tobiifree).
	if _, err = d.dev.Control(reqTypeVendorIfaceOut, reqSessionOpen, 0, uint16(conf.Interface), nil); err != nil {
		return nil, fmt.Errorf("session open (vendor ctrl 0x41): %w", err)
	}
	d.sessionOpen = true

	// N transfer IN sempre in coda nel kernel: tra la fine di una e la
	// successiva lettura il dispositivo trova comunque buffer pronti.
	if d.stream, err = in.NewStream(conf.TransferSize, conf.InFlight); err != nil {
		return nil, fmt.Errorf("in stream: %w", err)
	}
	log.Info("usb device opened",
		"vid", d.Info.VendorID.String(), "pid", d.Info.ProductID.String(),
		"bus", d.Info.Bus, "addr", d.Info.Address, "speed", d.Info.Speed,
		"product", d.Info.Product, "serial", d.Info.Serial,
		"transfer_size", conf.TransferSize, "in_flight", conf.InFlight)
	return d, nil
}

// Read blocca finché una bulk transfer IN non completa (risveglio dal kernel,
// nessun polling) o finché ctx non viene cancellato. Restituisce i byte di
// UNA transfer. Non è safe per uso concorrente: va chiamata da una sola goroutine.
func (d *Device) Read(ctx context.Context, buf []byte) (int, error) {
	return d.stream.ReadContext(ctx, buf)
}

// Write invia un messaggio già serializzato su EP OUT.
func (d *Device) Write(ctx context.Context, frame []byte) error {
	ctx, cancel := context.WithTimeout(ctx, d.conf.WriteTimeout)
	defer cancel()
	n, err := d.out.WriteContext(ctx, frame)
	if err != nil {
		return fmt.Errorf("bulk out: %w", err)
	}
	if n != len(frame) {
		return fmt.Errorf("bulk out: short write %d/%d", n, len(frame))
	}
	return nil
}

// Close chiude la sessione TTP e rilascia tutto. Va chiamata solo dopo che la
// goroutine di lettura è terminata.
func (d *Device) Close() {
	if d.stream != nil {
		d.stream.Close()
		d.stream = nil
	}
	if d.sessionOpen && d.dev != nil {
		if _, err := d.dev.Control(reqTypeVendorIfaceOut, reqSessionClose, 0, uint16(d.conf.Interface), nil); err != nil {
			d.log.Debug("session close failed", "err", err)
		}
		d.sessionOpen = false
	}
	if d.intf != nil {
		d.intf.Close()
		d.intf = nil
	}
	if d.cfg != nil {
		d.cfg.Close()
		d.cfg = nil
	}
	if d.dev != nil {
		d.dev.Close()
		d.dev = nil
	}
	if d.ctx != nil {
		d.ctx.Close()
		d.ctx = nil
	}
}

// IsDisconnect indica un errore che implica la scomparsa del dispositivo.
func IsDisconnect(err error) bool {
	return errors.Is(err, gousb.ErrorNoDevice) || errors.Is(err, gousb.ErrorIO) ||
		errors.Is(err, gousb.TransferNoDevice) || errors.Is(err, gousb.ErrorPipe) ||
		errors.Is(err, gousb.TransferStall)
}

// isBusy riconosce LIBUSB_ERROR_BUSY anche quando gousb non lo avvolge:
// Config.Interface lo formatta con %v ("failed to claim interface …: libusb:
// device or resource busy [code -6]"), quindi errors.Is da solo non basta.
func isBusy(err error) bool {
	return errors.Is(err, gousb.ErrorBusy) || strings.Contains(err.Error(), gousb.ErrorBusy.Error())
}
