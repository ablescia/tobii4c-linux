// Package ttp implementa il framing del Tobii Tracker Protocol (TTP) così come
// viaggia sugli endpoint bulk dell'interfaccia vendor (interfaccia 0) dei
// tracker Tobii basati su EyeChip (IS4 = Tobii 4C, IS5 = Eye Tracker 5).
//
// Layout di un messaggio sul filo:
//
//		envelope (8 B, little-endian)       header TTP (24 B, BIG-endian)        payload
//		[dir 0 0 0][len u32 LE]             [marker][seq][status][op][error][plen]  [00 00][TLV...]
//
//	  - dir = 0x00 host→device (EP 0x05), 0x01 device→host (EP 0x83).
//	  - len: in uscita conta i byte DOPO l'envelope, in ingresso conta il
//	    messaggio INTERO (envelope compreso). L'asimmetria è reale.
//	  - Tutte le word dell'header e tutti i campi TLV sono big-endian: l'unico
//	    campo little-endian del protocollo è la lunghezza nell'envelope.
//
// Fonti: Tropaion/Tobii_Linux (wiki TTP-Framing, USB-Transport),
// Krazubu/tobii-linux (crates/tobii-proto), Aetherall/tobiifree; descrittori
// USB letti dal 4C reale (2104:0127).
package ttp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Dimensioni fisse del framing.
const (
	EnvelopeSize = 8
	HeaderSize   = 24
	FullHeader   = EnvelopeSize + HeaderSize // 32: il payload inizia qui

	dirOut = 0x00
	dirIn  = 0x01
)

// Marker (prima word dell'header TTP).
const (
	MarkerRequest      uint32 = 0x51 // host → device
	MarkerResponse     uint32 = 0x52 // risposta, stesso seq della richiesta
	MarkerStream       uint32 = 0x53 // campione di uno stream sottoscritto (op = stream id)
	MarkerNotification uint32 = 0x4e // notifica asincrona (op = id notifica)
)

// Op code noti. Valori decimali tra parentesi: sono quelli che compaiono negli
// errori di Talon ("command 0x42e" = 1070 = CALIBRATE_POINTS_APPLY).
const (
	OpHello           uint32 = 0x3e8 // (1000) versione di protocollo
	OpCalPointsApply  uint32 = 0x42e // (1070) ← l'op dell'errore Talon EyeCmdErr
	OpStreamCatalogue uint32 = 0x4b0 // (1200) elenco stream id → nome
	OpStreamColumns   uint32 = 0x4ba // (1210) elenco colonne (chiave → nome) di uno stream
	OpStreamStart     uint32 = 0x4c4 // (1220) subscribe
	OpStreamStop      uint32 = 0x4ce // (1230) unsubscribe
	OpProperties      uint32 = 0x532 // (1330)
	OpTrackBox        uint32 = 0x578 // (1400)
	OpDeviceStrings   uint32 = 0x58c // (1420) seriale, modello, generazione, firmware
	OpDisplayAreaGet  uint32 = 0x596 // (1430)
	OpDisplayAreaSet  uint32 = 0x5a0 // (1440)
	OpStatus          uint32 = 0x5d2 // (1490)
	OpQueryRealm      uint32 = 0x640 // (1600)
	OpOutputRate      uint32 = 0x672 // (1650)
	OpGetEnabledEye   uint32 = 0xc62 // (3170)
)

// Stream id noti (catalogo dell'ET5; il 4C va interrogato con OpStreamCatalogue).
const (
	StreamGaze     uint32 = 0x500
	StreamImage    uint32 = 0x501
	StreamPresence uint32 = 0x504
)

// Codici errore TTP (word "error" della risposta).
const (
	ErrNone             uint32 = 0
	ErrNotSupported     uint32 = 0x2000_0501
	ErrUnauthorized     uint32 = 0x2000_0505
	ErrBadState         uint32 = 0x2000_0508
	ErrInvalidParameter uint32 = 0x2000_0509
)

// TTPError è un rifiuto esplicito del dispositivo.
type TTPError struct {
	Op   uint32
	Code uint32
}

func (e *TTPError) Error() string {
	name := "unknown"
	switch e.Code {
	case ErrNotSupported:
		name = "NOT_SUPPORTED"
	case ErrUnauthorized:
		name = "UNAUTHORIZED"
	case ErrBadState:
		name = "BAD_STATE"
	case ErrInvalidParameter:
		name = "INVALID_PARAMETER"
	}
	return fmt.Sprintf("ttp: op 0x%x refused with error 0x%08x (%s)", e.Op, e.Code, name)
}

// Header è la vista decodificata delle 6 word TTP. Value type, nessuna allocazione.
type Header struct {
	Marker     uint32
	Seq        uint32
	Status     uint32 // 1 su ogni risposta osservata
	Op         uint32 // comando, stream id o id notifica
	Error      uint32 // 0 = ok
	PayloadLen uint32
}

// Errori di parsing.
var (
	ErrShort        = errors.New("ttp: buffer too short")
	ErrBadDirection = errors.New("ttp: bad envelope direction byte")
)

// ParseHeader legge envelope+header da un messaggio device→host completo.
// Restituisce l'header e il payload (sottoslice di msg, nessuna copia).
func ParseHeader(msg []byte) (Header, []byte, error) {
	if len(msg) < FullHeader {
		return Header{}, nil, ErrShort
	}
	if msg[0] != dirIn {
		return Header{}, nil, ErrBadDirection
	}
	be := binary.BigEndian
	h := Header{
		Marker:     be.Uint32(msg[8:]),
		Seq:        be.Uint32(msg[12:]),
		Status:     be.Uint32(msg[16:]),
		Op:         be.Uint32(msg[20:]),
		Error:      be.Uint32(msg[24:]),
		PayloadLen: be.Uint32(msg[28:]),
	}
	return h, msg[FullHeader:], nil
}

// MaxOutPiece è il corpo massimo di una singola bulk OUT, come la invia lo
// Stream Engine Windows (scritture da 4095 byte = 8 di envelope + 4087).
const MaxOutPiece = 4087

// AppendRequest serializza una richiesta (envelope + header + payload) in dst
// e restituisce lo slice esteso. Con un dst a capacità sufficiente non alloca.
// Valido solo per payload che stanno in un'unica bulk (≤ MaxOutPiece-24 byte):
// tutti i comandi usati da questo driver lo sono.
func AppendRequest(dst []byte, op, seq uint32, payload []byte) []byte {
	ttpLen := uint32(HeaderSize + len(payload))
	var hdr [FullHeader]byte
	hdr[0] = dirOut
	binary.LittleEndian.PutUint32(hdr[4:], ttpLen)
	be := binary.BigEndian
	be.PutUint32(hdr[8:], MarkerRequest)
	be.PutUint32(hdr[12:], seq)
	be.PutUint32(hdr[16:], 0)
	be.PutUint32(hdr[20:], op)
	be.PutUint32(hdr[24:], 0)
	be.PutUint32(hdr[28:], uint32(len(payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}
