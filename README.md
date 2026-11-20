# tobii4c-linux

Driver user-space per il **Tobii Eye Tracker 4C** su Linux, scritto in Go:
niente modulo del kernel e niente binari proprietari Tobii.

> **In English.** `tobii4c` is a user-space Linux driver for the Tobii Eye
> Tracker 4C (USB `2104:0127`), written in Go. It needs no kernel module and
> none of Tobii's proprietary binaries: it speaks the reconstructed TTP
> protocol directly over USB through libusb. At 90 Hz it streams gaze, eye
> positions, pupil diameter, head pose and user presence to other programs,
> over local UDP (a documented 192-byte little-endian packet) and,
> optionally, Lab Streaming Layer. It ships a full-screen calibration app
> (sensor placement, 7-point calibration, validation) and runs as a systemd
> user service. The 4C has no head pose stream, so head pitch is estimated
> from the tracker's IR camera with MediaPipe's face landmark model, run by a
> small pure-Go ONNX interpreter. Tested on one 4C with firmware 4.7.0.
> The documentation below is in Italian; the
> [UDP packet layout](#formato-udp-t4cg-v2) and the
> [`examples/recv.py`](examples/recv.py) receiver are enough to read the data.
>
> ```sh
> make install install-udev install-service   # binary, udev rule, systemd user unit
> tobii4c calibrate                           # full-screen calibration (X11/XWayland)
> systemctl --user enable --now tobii4c       # stream to UDP 127.0.0.1:5005
> tobii4c recv                                # check that samples arrive
> ```

Il driver parla il protocollo TTP direttamente sugli endpoint bulk USB, tramite
[`google/gousb`](https://github.com/google/gousb) (libusb-1.0). Ogni 11 ms
circa produce un campione con sguardo, posizione degli occhi (nel sistema del
tracker, in quello dello schermo e nel track box), diametro delle pupille,
posa della testa e presenza dell'utente, e lo inoltra ad altri programmi via
**UDP locale** e, se serve, via **Lab Streaming Layer**. Il pitch della testa,
che i soli occhi non permettono di ricavare, viene stimato dalle immagini IR
del sensore (vedi [Pitch della testa](#pitch-della-testa)).

## Funzionalità

- **Streaming a 90 Hz** di sguardo (filtrato e non filtrato), centri
  corneali, pupille, posa della testa e presenza, con una latenza della
  pipeline sotto i 100 µs e nessuna allocazione durante lo streaming.
- **Dati per altri programmi**: datagrammi UDP nel formato T4CG v2,
  [documentato qui sotto](#formato-udp-t4cg-v2) con un ricevitore Python di
  esempio, e un outlet LSL facoltativo.
- **Calibrazione** a schermo intero con gli stessi passi dell'applicazione
  ufficiale Tobii: posizionamento del sensore e display area, posizione
  dell'utente, 7 punti, verifica con accuratezza e precisione per punto.
  Backup e ripristino della calibrazione precedente.
- **Servizio utente systemd** che a ogni connessione riscrive display area e
  calibrazione salvate e si riconnette da solo dopo un hotplug.
- **Pitch della testa** dalla camera IR del 4C, con una calibrazione della
  camera guidata da fare una volta sola.
- **Senza root**: una regola udev dà l'accesso al tracker all'utente della
  sessione locale.

## Stato

Misure del 2026-11-03 su un 4C con firmware `IS4 4.7.0-d374125`
(`IS4_Large_Peripheral`), Ryzen 7 2700X, kernel 6.12:

| Metrica | Valore misurato |
|---|---|
| Frequenza dello stream gaze | **90,0 Hz** (op 0x672 dichiara 90/90) |
| Latenza della pipeline (ricezione USB → sink scritto) | p50 **45 µs**, p99 ~80 µs, max < 100 µs |
| Età del campione al ricevitore UDP | 50–140 µs |
| Decodifica di un frame (1692 B, 39 colonne) | 1,2 µs, **0 allocazioni** |
| Cicli di GC durante 12 s di streaming | **0** |
| Campioni persi / fuori ordine (1065 via UDP, 801 con 8 worker) | **0 / 0** |
| Data race (`go build -race`, 8 worker, canale non bufferizzato) | nessuna |
| Hotplug (de-autorizzazione USB simulata) | disconnessione rilevata in 3 ms, riconnessione 32 ms dopo il ritorno del dispositivo |
| LSL (inlet reale `tobii4c lsl-recv`, liblsl 1.17.7) | 27 canali, 1410 campioni in 15 s = 90,07 Hz, nessuna pausa, ritardo mediano 0,14 ms; pitch valido nel 100% dei campioni con l'utente davanti |
| Calibrazione a 7 punti (`tobii4c calibrate`, 3 sessioni reali) | accuratezza media verificata **0,61°–0,92°** su 5 punti di verifica |

Compatibilità:

- **Dispositivo**: provato su un solo esemplare di 4C (firmware 4.7.0). Se un
  firmware diverso dichiara colonne diverse, il driver lo segnala nel log
  all'avvio. Il driver accetta anche l'id dell'Eye Tracker 5 (`2104:0313`),
  che usa la stessa famiglia di protocollo, ma sull'ET5 non è stato provato.
- **Sistema**: Debian 13 e Arch Linux (Omarchy). Serve solo libusb-1.0 e,
  per le finestre di calibrazione, una sessione grafica X11 oppure Wayland
  con XWayland.
- **CPU**: il modello del viso usa kernel AVX2/FMA su amd64, con un ripiego
  in Go puro sulle altre CPU.

## Avvio rapido

```sh
# 1. dipendenze (Debian/Ubuntu; per Arch vedi sotto) e sorgenti
sudo apt install git make golang libusb-1.0-0-dev pkg-config
git clone https://github.com/ablescia/tobii4c-linux.git && cd tobii4c-linux

# 2. binario in ~/.local/bin, regola udev (chiede sudo), unit systemd utente
make install install-udev install-service

# 3. con il 4C collegato e montato sotto lo schermo: configurazione e calibrazione
tobii4c calibrate

# 4. avvio del driver come servizio, anche ai login successivi
systemctl --user enable --now tobii4c

# 5. controllo: i campioni arrivano su UDP 127.0.0.1:5005
tobii4c recv
```

`make install-udev` applica subito la regola anche al tracker già collegato
(`udevadm trigger`). Se `tobii4c` non viene trovato, `~/.local/bin` non è nel
`PATH` (vedi [Dipendenze e compilazione](#dipendenze-e-compilazione)).

## Installazione

### Dipendenze e compilazione

```sh
# Debian / Ubuntu:
sudo apt install git make golang libusb-1.0-0-dev pkg-config   # Go ≥ 1.22
# Arch / Omarchy:
sudo pacman -S --needed git base-devel go libusb pkgconf       # Go ≥ 1.22
git clone https://github.com/ablescia/tobii4c-linux.git
cd tobii4c-linux
make build          # → bin/tobii4c
make test           # vet + unit test (non richiedono il dispositivo)
make install        # → ~/.local/bin/tobii4c (PREFIX=… per un'altra cartella)
```

Su un'installazione di base di Debian 13 mancano sia `git` sia `make`.
`golang` porta Go 1.24 e, tra i pacchetti raccomandati, `gcc`, che serve a cgo
per libusb. Su Arch (e quindi su Omarchy) `base-devel` fornisce `make`, `gcc` e
`pkgconf`, mentre il pacchetto `libusb` include gli header di libusb-1.0.
Niente nel driver dipende dalla distribuzione, solo queste dipendenze e, a
runtime, udev e systemd.

Debian mette `~/.local/bin` nel `PATH` dalla login successiva, se la cartella
esiste; su Arch/Omarchy dipende dalla shell e dal profilo, quindi conviene
verificarlo con `echo $PATH`. `make uninstall` toglie il binario.

In alternativa, senza clonare (servono comunque Go, libusb-1.0 con i suoi header
e pkg-config):

```sh
go install github.com/ablescia/tobii4c-linux/cmd/tobii4c@latest
export PATH="$PATH:$(go env GOPATH)/bin"   # ~/go/bin, che né Debian né Arch mettono nel PATH
```

Il modello del viso per il pitch della testa (4,9 MB) è nel repository
(`internal/facemodel`) e viene incorporato nel binario con `go:embed`: non va
scaricato né installato.

### Permessi USB senza root (udev)

Il driver non richiede root: una regola udev assegna il nodo
`/dev/bus/usb/BBB/DDD` all'utente a ogni hotplug. La regola è in
`udev/70-tobii-4c.rules` ed è incorporata anche nel binario:

```sh
make install-udev
# equivale a:
tobii4c udev | sudo tee /etc/udev/rules.d/70-tobii-4c.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104
```

La regola usa `TAG+="uaccess"`: systemd-logind concede un'ACL `rw` all'utente
della sessione locale attiva. Il prefisso `70-` è obbligatorio, perché
`uaccess` viene applicato da `73-seat-late.rules`. Per verificarla (`getfacl` è
nel pacchetto `acl`; su Arch `lsusb` è nel pacchetto `usbutils`):

```sh
getfacl /dev/bus/usb/$(lsusb -d 2104:0127 | awk '{print $2"/"substr($4,1,3)}')
# deve comparire user:<utente>:rw-
```

L'ACL c'è solo finché l'utente ha una **sessione locale attiva**: un desktop
(anche un compositor Wayland come Hyprland) o un login sulla console. Una
sessione SSH da sola non basta, e il driver risponde `libusb: bad access`.
Per un sistema headless o usato solo via SSH c'è nel file, commentata, la
variante `GROUP="plugdev", MODE="0660"`. Su Debian il primo utente è già nel
gruppo `plugdev` (verifica con `id`); su Arch il gruppo non esiste di default
e va creato (ha effetto dal login successivo):

```sh
sudo groupadd -f plugdev && sudo gpasswd -a "$USER" plugdev   # solo Arch: il gruppo non c'è
sudo sed -i 's/^#\(SUBSYSTEM.*0127.*GROUP="plugdev"\)/\1/' /etc/udev/rules.d/70-tobii-4c.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104
```

Nel file c'è anche `MODE="0666"`, permissiva e sconsigliata su macchine
multiutente. `make uninstall-udev` toglie la regola.

Il detach del driver kernel avviene tramite `SetAutoDetach(true)` di gousb.
Sull'interfaccia 0 di solito non è legato nessun driver; al rilascio libusb
ricollega quello dell'interfaccia reclamata.

### Servizio systemd

```sh
make install install-service        # binario e ~/.config/systemd/user/tobii4c.service
systemctl --user enable --now tobii4c
journalctl --user -u tobii4c -f     # log JSON
```

L'unit (`systemd/tobii4c.service`) è un servizio utente: non serve root e
parte al login. Esegue:

```sh
tobii4c run -udp 127.0.0.1:5005 -display-mm saved -calibration saved -log-format json
```

Il 4C **dimentica display area e calibrazione quando resta senza
alimentazione** (vedi [Note sul protocollo](#note-sul-protocollo)). Con
`saved` il driver le riscrive a ogni connessione, anche dopo un hotplug,
leggendole da `~/.config/tobii4c`, dove le salva `tobii4c calibrate`. Se i
file non ci sono ancora, lo scrive nel log e parte comunque.

`make install-service` sostituisce nel file il percorso del binario con quello
di `$(PREFIX)`. Non abilita il servizio: `make uninstall-service` lo ferma e
lo toglie.

Il servizio tiene il tracker in **uso esclusivo**. Prima di `calibrate`,
`headpose-calibrate`, `info`, `display` o `calibration` va fermato, e poi
riavviato:

```sh
systemctl --user stop tobii4c
tobii4c calibrate
systemctl --user start tobii4c
```

Altrimenti questi comandi rispondono `device or resource busy`. Lo stesso vale
per i programmi che avviano `tobii4c run` da sé: in quel caso il servizio
non serve e va lasciato disabilitato.

## Calibrazione

Le calibrazioni sono due e diverse: una riguarda la persona che usa il
tracker, l'altra il dispositivo.

| | Sguardo (`tobii4c calibrate`) | Camera e zero del pitch (`tobii4c headpose-calibrate`) |
|---|---|---|
| Serve a | sguardo e posizione degli occhi | solo il pitch della testa |
| Chi la fa | chi userà il tracker, con i propri occhi | una persona qualsiasi, una volta per ogni 4C |
| Quando rifarla | nuova persona, altro monitor, sensore spostato, occhiali o lenti a contatto | un altro 4C; lo zero del pitch per un'altra persona |
| File | `calibration.bin`, `display.json` | `camera.json`, `headpose-user.json` |

Entrambe aprono una finestra a schermo intero sul monitor primario
(`-monitor HDMI-0` per sceglierne un altro, `calibrate -monitors` per
elencarli) e richiedono una sessione grafica: senza, terminano con
`connessione a X11 ($DISPLAY)`. Su Wayland funzionano tramite XWayland.

### Sguardo: `tobii4c calibrate`

Ripropone i passi dell'applicazione ufficiale Tobii:

1. **Posiziona il sensore.** Due frecce sul bordo inferiore dello schermo vanno
   allineate ai due segni guida sul tracker. Il 4C dichiara (op 2110) 2 segni a
   184 mm e una camera inclinata di 20°. Se l'EDID del monitor è credibile, la
   dimensione viene da lì e le frecce si spostano insieme, altrimenti (o
   premendo M) la loro distanza in pixel dà il passo del pixel. La display
   area si calcola con la stessa formula dello Stream Engine
   (`tobii_calculate_display_area_basic`), verificata sulle catture Windows
   sotto il micrometro, e si salva in `~/.config/tobii4c/display.json`.
2. **Controlla la tua posizione.** Vista dal vivo degli occhi (a specchio),
   scala della distanza con la zona 50–80 cm, indicazioni per avvicinarsi,
   allontanarsi o centrarsi.
3. **Calibra lo sguardo.** Sono i 7 punti dell'app Windows nello stesso ordine,
   a batch {1, 3, 3} con un compute dopo ogni batch. Prima di ogni raccolta il
   programma aspetta che lo sguardo resti per 0,5 s entro 80 mm dal punto
   (ammiccamenti tollerati). Fa eccezione il primo punto: dopo il clear il 4C
   non fornisce sguardo finché il primo compute non produce una calibrazione,
   quindi lì si raccoglie dopo 0,9 s di sosta con gli occhi visibili. Se
   durante la raccolta gli occhi si perdono, il punto viene scartato (op 1080)
   e ripresentato, fino a 3 tentativi.
4. **Verifica.** 5 punti diversi da quelli di calibrazione. Per ciascuno si
   calcolano l'accuratezza (angolo tra bersaglio e sguardo medio visto
   dall'occhio, in gradi) e la precisione (RMS campione-campione). Il
   risultato mostra bersagli, campioni ed errore per punto. Da lì si può
   ricalibrare, ripristinare la calibrazione precedente (P) o provare lo
   sguardo dal vivo (G).

Basta guardare i punti, la testa non si deve muovere; i tasti (Spazio, Invio)
può premerli chi assiste. Prima di iniziare, la calibrazione attiva viene
salvata in `~/.config/tobii4c/backups/`, e `Esc`, `Ctrl-C` o un errore la
rimettono sul dispositivo. La nuova calibrazione è attiva subito dopo il
compute e viene salvata in `~/.config/tobii4c/calibration.bin`. Con più
persone si tiene un file per ciascuna: `tobii4c calibration save -f
persona1.bin`, poi `tobii4c calibration apply -f persona1.bin`.

Misure sul 4C reale. Un collect blocca **~665 ms** (~1,31 s sui due angoli in
alto). Un compute dura 65 ms dopo 1 punto, ~830 ms dopo 4 e ~1,6 s dopo 7. Ogni
compute è seguito dalla notifica 3220 col nuovo id di calibrazione; al
ripristino torna l'id originale. Durante start, clear, compute e scrittura del
blob il 4C sospende lo stream dello sguardo (da 15 a 144 frame): il driver li
conta come `device_pauses` e non come frame persi. L'accuratezza si misura
sullo sguardo filtrato (0x1c, quello inviato via UDP), la precisione su quello
non filtrato (0x20), perché il filtro del firmware la farebbe sembrare ~0,01°.

La finestra è X11 nativa, in Go puro, e non una pagina web: il Fullscreen API
dei browser mostra per alcuni secondi il banner "premi Esc per uscire" proprio
dove cade il punto in alto al centro. La finestra si posiziona via RandR
esattamente sul monitor scelto, prende la tastiera e nasconde il cursore.

**Senza tracker.** `tobii4c calibrate -simulate` esegue tutto il flusso con
occhi sintetici, sguardo con latenza saccadica, rumore, errore sistematico che
il compute riduce e ammiccamenti. Funziona anche su un display virtuale
(`xvfb` su Debian, `xorg-server-xvfb` su Arch):
`Xvfb :99 & DISPLAY=:99 tobii4c calibrate -simulate`.

### Camera e zero del pitch: `tobii4c headpose-calibrate`

Serve solo al pitch della testa: chi non lo usa può saltarla
(`tobii4c run -headpose off`), lo sguardo funziona comunque. Misura la camera
IR del 4C (focale, centro ottico e posizione rispetto al tracker), che il
firmware non espone. Sono proprietà dell'hardware: basta che **una persona la
faccia una volta** per ogni 4C, per esempio chi installa il sistema. Il menu
ha tre scelte:

1. **calibrazione completa**: ci si muove con calma finché si riempiono le
   caselle della distanza (50–85 cm), di lato (±20 cm) e in altezza (±6 cm),
   ognuna con un secondo e mezzo di permanenza. A testa ferma i parametri
   della camera non si possono ricavare. Il risultato è accettato solo se
   l'errore di riproiezione è sotto 4 px e l'incertezza della rotazione sotto
   1°; Invio lo salva in `~/.config/tobii4c/camera.json`, legato al numero di
   serie del 4C. Seguono lo zero del pitch, la sua verifica e quella della
   presenza (saltabile con S). Ripetere le fasi aggiunge campioni invece di
   ripartire da zero.
2. **solo il viso**: misura lo **zero personale del pitch**, cioè il pitch con
   la testa dritta mentre si guarda il punto al centro, e lo salva in
   `~/.config/tobii4c/headpose-user.json`. I passi successivi, con il mento in
   su e in giù, sono solo un controllo e vanno avanti da soli. Lo zero
   appartiene alla persona: se la calibrazione completa l'ha fatta qualcun
   altro, l'utente rifà questo passo.
3. **solo verifica**: pitch e presenza, senza salvare nulla.

Su un altro computer con lo stesso 4C basta copiare `camera.json`. Senza
l'applicazione, `tobii4c run` calibra la camera da solo mentre l'utente si
muove davanti al tracker; il log `head pose: camera calibration not yet
reliable` dice quanto manca.

### File di configurazione

Tutto sta in `~/.config/tobii4c/` (`$XDG_CONFIG_HOME/tobii4c`):

| File | Scritto da | Contenuto |
|---|---|---|
| `calibration.bin` | `calibrate`, `calibration save` | blob di calibrazione del 4C (centinaia di KB), riapplicato da `run -calibration saved` |
| `display.json` | `calibrate`, `display -set` | geometria dello schermo: `width_mm`, `height_mm`, `offset_x_mm`, `monitor`, `area` (angoli in mm nel sistema del tracker), riapplicata da `run -display-mm saved` |
| `camera.json` | `headpose-calibrate`, `run` | calibrazione della camera IR, con il numero di serie del 4C |
| `headpose-user.json` | `headpose-calibrate` | zero personale del pitch |
| `backups/` | `calibrate`, `calibration apply` | calibrazioni sostituite, la più recente per prima |

```sh
tobii4c calibration save     # blob attivo sul 4C → calibration.bin (-f per un altro file)
tobii4c calibration apply    # file → 4C, dopo un backup di quella attiva
tobii4c calibration restore  # rimette l'ultimo backup
tobii4c display              # mounting e display area attuali
tobii4c display -set 698x393 -offset-x 0   # scrive la display area e la salva in display.json
```

## Leggere i dati da altri programmi

### Formato UDP T4CG v2

`tobii4c run` invia di default a `127.0.0.1:5005` un datagramma di
**192 byte** per campione, tutto **little-endian**. I primi 120 byte hanno gli
stessi offset della v1; i campi nuovi sono accodati.

| Offset | Tipo | Campo |
|---:|---|---|
| 0 | `char[4]` | magic `"T4CG"` |
| 4 | `u8` | versione (2) |
| 5 | `u8` | sorgente della posa della testa: 0 nessuna, 1 derivata, 2 dispositivo |
| 6 | `u16` | flag di validità (bit 0 gaze, 1 gaze_raw, 2/3 occhio L/R tracciato, 4/5 origine L/R, 6 posizione testa, 7 pitch, 8 yaw, 9 roll, 10/11 pupilla L/R, 12/13 gaze 2D L/R) |
| 8 | `u64` | `seq`: monotono, senza buchi tra campioni inoltrati, continuo tra riconnessioni |
| 16 | `i64` | `host_ns`: istante di ricezione USB (CLOCK_REALTIME) |
| 24 | `i64` | `device_us`: orologio del tracker |
| 32 | `f32×2` | sguardo combinato filtrato, normalizzato [0,1] sulla display area (0,0 = in alto a sinistra) |
| 40 | `f32×2` | sguardo combinato non filtrato ("responsive") |
| 48 | `f32×3` | posizione della testa, mm **dal centro dello schermo** (+X destra, +Y su, +Z verso l'utente) |
| 60 | `f32×3` | rotazione della testa, rad (pitch, yaw, roll) attorno agli assi dello schermo; il pitch è valido (bit 7) solo con la stima dalle immagini attiva, che di default fornisce anche yaw e roll (flag v2, bit 6/7/8) |
| 72 / 84 | `f32×3` | centro corneale sinistro / destro, mm, sistema del tracker |
| 96 / 100 | `f32` | diametro pupilla sinistra / destra, mm |
| 104 / 112 | `f32×2` | punto di sguardo 2D sinistro / destro |
| 120 | `u32` | flag di validità v2 (bit 0/1 origine schermo L/R, 2/3 track box L/R, 4/5 apertura L/R, 6/7/8 pitch/yaw/roll della testa dal viso nelle immagini invece che dagli occhi) |
| 124 | `u8` | presenza: 0 sconosciuta, 1 assente, 2 presente (+ 3 byte a zero) |
| 128 | `i64` | `presence_us`: orologio del tracker all'ultimo messaggio di presenza (0 = nessuno) |
| 136 / 148 | `f32×3` | centro corneale sinistro / destro, mm dal centro dello schermo (= `gaze_origin` dello Stream Engine) |
| 160 / 172 | `f32×3` | posizione dell'occhio sinistro / destro nel track box, normalizzata [0,1] (= `eye_position_normalized`) |
| 184 / 188 | `f32` | apertura della palpebra sinistra / destra, mm |

```python
struct.unpack('<4sBBHQqq2f2f3f3f3f3fff2f2fIB3xq3f3f3f3fff', data)
```

Per i pixel basta moltiplicare lo sguardo per la risoluzione del monitor
corrispondente alla display area. Valori fuori da [0,1] indicano uno sguardo
fuori schermo, mentre `(-1,-1)` è la sentinella del dispositivo per "nessuno
sguardo". **Usate sempre i flag di validità**: quando non vede un occhio, il
dispositivo invia comunque i campi, azzerati (o a −1 per pupille e apertura).
L'apertura ha la validità del firmware e non dipende dal tracciamento
dell'occhio.

Rispetto alla v1, oltre ai campi accodati, la posa della testa è nel sistema
dello schermo e non più in quello del tracker, e i bit 12/13 seguono il
tracciamento dell'occhio, perché la colonna 0x1d/0x1e usata prima è la
validità del track box.

Due ricevitori di prova, che leggono sia la v1 sia la v2:

```sh
tobii4c recv                   # in Go: campione, frequenza ed età del dato
python3 examples/recv.py       # in Python, solo libreria standard: base per un'integrazione
```

### Lab Streaming Layer (facoltativo)

L'outlet LSL è dietro il build tag `lsl` e richiede
[liblsl](https://github.com/sccn/liblsl) (header `lsl_c.h` e `liblsl.so`).
Debian non la pacchettizza, ma il `.deb` ufficiale per Ubuntu 24.04 funziona
anche su Debian 13 (provato con la 1.17.7). Su Arch liblsl è nell'AUR
(pacchetto `liblsl`).

```sh
wget -P /tmp https://github.com/sccn/liblsl/releases/download/v1.17.7/liblsl-1.17.7-noble_amd64.deb
echo "9cac7570c2b256077b86c011ffa44b7a40344f2271cc353120c387fb9134ddf1  /tmp/liblsl-1.17.7-noble_amd64.deb" | sha256sum -c -
sudo apt install /tmp/liblsl-1.17.7-noble_amd64.deb
make install-lsl                     # come make install, ma con -tags lsl
tobii4c run -lsl &                   # outlet "Tobii4C"
tobii4c lsl-recv -duration 10s       # ricevitore di prova: canali, frequenza, NaN, ritardo
kill %1                              # ferma l'outlet, che tiene occupato il tracker
```

`make install` ricompila sempre senza LSL. Per il servizio, aggiungere `-lsl`
alla riga `ExecStart` dell'unit (e `-udp ""` se l'UDP non serve).

L'outlet ha 27 canali float32. I primi 12 sono quelli della v1, con validità
esplicita: `gaze_x, gaze_y, gaze_valid, head_x_mm, head_y_mm, head_z_mm,
head_pitch_rad, head_yaw_rad, head_roll_rad, head_valid, pupil_left_mm,
pupil_right_mm`. Seguono `presence` (0/1/2), `origin_{left,right}_{x,y,z}_mm`
(sistema dello schermo), `trackbox_{left,right}_{x,y,z}` e
`openness_{left,right}_mm`, che valgono NaN quando il dato non è valido, come
gli angoli della testa. `head_valid` vale per la posizione: yaw e roll dal
viso possono essere validi anche quando il tracker vede un occhio solo. Il
timestamp viene retrodatato all'istante di ricezione USB (`lsl_local_clock() −
latenza pipeline`).

### Integrare il driver in un'applicazione

Un programma che usa il 4C può limitarsi a leggere l'UDP del servizio, oppure
gestire il driver da sé. In questo caso si appoggia a quattro interfacce, che
il progetto tratta come stabili:

1. **I campioni**: il pacchetto T4CG v2 descritto sopra, su un indirizzo
   scelto con `tobii4c run -udp 127.0.0.1:<porta>`.
2. **Il log di `run`**: con `-log-format json` il driver scrive su stderr un
   oggetto JSON per riga (`time`, `level`, `msg` e attributi). I messaggi utili
   per seguire lo stato:

   | `msg` | Attributi | Significato |
   |---|---|---|
   | `udp output` | `addr`, `format` | uscita UDP aperta |
   | `usb device opened` | `bus`, `addr`, `serial` | tracker aperto |
   | `device identity` | `strings` (seriale \| modello \| generazione \| firmware) | identità del firmware |
   | `display area (device)`, `display area set` | `area` | display area letta, o appena scritta |
   | `calibration applied`, `calibration apply failed` | `bytes`, `err` | esito di `-calibration` |
   | `subscribed` | `stream` (`0x500 gaze` per lo sguardo) | lo streaming è iniziato |
   | `usb device disconnected`, `session ended, reconnecting` | `err` | tracker perso; il driver riprova da solo |

   Se il tracker non si apre, `err` contiene `no Tobii tracker found`,
   `busy` (un altro processo lo tiene) o `access` (regola udev o sessione
   locale mancanti). Un errore fatale termina il processo con codice 1 e la
   riga `errore: …` su stderr.
3. **La calibrazione**: `tobii4c calibrate -log-format json [-monitor NOME]`
   apre la sua finestra e termina quando l'utente la chiude. Nel log:
   `calibration started`, `validation point` (`error_deg`, `precision_deg`),
   `calibration finished`, `calibration aborted` (`restored_previous`). Un
   `SIGINT` equivale a `Esc`: annulla la calibrazione in corso e rimette
   quella precedente prima di chiudere il tracker. `tobii4c
   headpose-calibrate [-monitor NOME]` si lancia allo stesso modo e chiude
   anch'esso con `SIGINT`.
4. **I file** di `~/.config/tobii4c` descritti in
   [File di configurazione](#file-di-configurazione), per esempio
   `display.json` per convertire lo sguardo normalizzato in millimetri.

Il tracker si apre in **esclusiva**: prima di lanciare `calibrate`, il
programma deve fermare il proprio `tobii4c run` e, se attivo, il servizio
`tobii4c.service`.

Programmi che usano già il driver in questo modo: **Kite**, una tastiera a
sguardo per la comunicazione aumentativa e alternativa (legge l'UDP e lancia
`calibrate`), e **tobii-ws** del progetto Tobii Race, un ponte WebSocket che
avvia `tobii4c run` come processo figlio e usa la posa della testa come
comando di gioco.

## Riferimento dei comandi

```sh
tobii4c run                 # sguardo, occhi, testa, presenza → UDP 127.0.0.1:5005
tobii4c run -print 500ms -stats 5s
tobii4c run -lsl -udp ""    # solo LSL (build con -tags lsl)
tobii4c calibrate           # configurazione e calibrazione dello sguardo
tobii4c headpose-calibrate  # camera del 4C e zero del pitch
tobii4c calibration save|apply|restore
tobii4c display             # mounting e display area; -set 698x393 -offset-x 0 per scriverla
tobii4c info                # identità, catalogo stream, colonne del firmware, display area
tobii4c info -probe -json /tmp/report.json
tobii4c recv                # ricevitore UDP di prova
tobii4c lsl-recv            # ricevitore LSL di prova (build con -tags lsl)
tobii4c udev                # stampa la regola udev
```

`tobii4c <comando> -h` elenca tutti i flag. Quelli principali di `run`:

| Flag | Default | Significato |
|---|---|---|
| `-udp` | `127.0.0.1:5005` | destinazione dei datagrammi (vuoto = off) |
| `-lsl`, `-lsl-name` | off, `Tobii4C` | outlet LSL, 27 canali float32 (build con `-tags lsl`) |
| `-print` | 0 | stampa un campione su stdout ogni intervallo |
| `-display-mm`, `-display-offset-x` | —, 0 | scrive la display area a ogni connessione, es. `698x393`, calcolata col mounting del tracker; `saved` = quella di `display.json` |
| `-calibration` | — | applica un blob a ogni connessione; `saved` = `calibration.bin` |
| `-presence` | on | sottoscrive lo stream presence (0x504) e lo riporta in ogni campione |
| `-headpose` | on | pitch della testa dalle immagini IR: `on`, `off` |
| `-headpose-rotation` | `image` | yaw e roll della testa: `image` (dal viso quando è tracciato, dagli occhi altrimenti), `eyes` (sempre dagli occhi) |
| `-headpose-rate` | 30 | inferenze al secondo al massimo (le immagini arrivano a 30 Hz) |
| `-headpose-workers` | 1 | goroutine dell'inferenza |
| `-headpose-images-always` | off | non sospende lo stream immagine quando l'utente è assente |
| `-headpose-camera` | `camera.json` | file della calibrazione della camera |
| `-stall` | 3s | senza frame: resubscribe, poi riconnessione |
| `-keepalive` | 5s | ping di salute (op 0x58c), RTT nei log debug |
| `-stats` | 10s | log periodico: Hz, latenza p50/p99/max, drop, resync, head pose |
| `-workers`, `-raw-queue`, `-frame-pool` | 2, 1, 64 | goroutine di decodifica, capacità del canale reader→framer, frame in volo prima di scartare |
| `-in-flight`, `-transfer-size` | 16, 65536 | URB IN in coda nel kernel e loro dimensione |
| `-log-level`, `-log-format` | info, text | `slog`; `json` per l'ingestione strutturata |

Il log riporta tra l'altro `usb device opened`, `subscribed`, `gaze columns
match keymap` (o un avviso se il firmware dichiara colonne diverse),
`presence` (cambio di stato), `device frames lost` (buchi nel frame counter),
`frame dropped: pipeline saturated`, `ttp stream resync`, `usb device
disconnected`, `gaze stream stalled` e lo `stats` periodico. Con il pitch:
`head pose model loaded`, `head pose: camera calibrated` / `saved` / `not yet
reliable`, `head pose` periodico, `image stream paused: user away` / `image
stream resumed`.

## Pitch della testa

Il firmware del 4C non calcola la posa della testa, e dai due soli centri
corneali si ricavano posizione, yaw e roll ma non il pitch. Il driver lo stima
dalle immagini IR dello stream 0x501, come fa lo Stream Engine con l'ET5:

1. **Ritaglio del viso**: dai landmark del frame precedente (come MediaPipe),
   oppure dagli occhi del tracker proiettati con la camera calibrata, oppure,
   se non c'è altro, con una ricerca a griglia su tutta l'immagine. La ricerca
   costa una trentina di inferenze: non parte mai quando lo stream presence
   dice che davanti non c'è nessuno, e si dirada fino a una volta ogni 5 s se
   continua a fallire. Il contrasto del ritaglio viene normalizzato (le
   immagini IR sono scure).
2. **Landmark**: rete `face_landmarks_detector` di MediaPipe Face Landmarker
   (478 punti 3D, ritaglio 256 × 256; Apache 2.0, © Google), convertita in
   ONNX, incorporata nel binario ed eseguita da `internal/nn`, un interprete in
   Go puro dei soli operatori che servono. Un'inferenza costa 113 milioni di
   moltiplicazioni-accumulo: 18–22 ms su un core del Ryzen 7 2700X, con
   AVX2/FMA.
3. **Posa nella camera**: fit di Procrustes pesato tra i landmark e il modello
   canonico del viso di MediaPipe (33 punti rigidi, pesi di MediaPipe), prima
   in prospettiva debole per la scala, poi con i punti retroproiettati lungo
   i raggi della camera.
4. **Sistema dello schermo**: camera → tracker con la calibrazione della
   camera, tracker → schermo con la display area. Gli angoli sono di Eulero
   R = Ry(yaw)·Rx(pitch)·Rz(roll), destrorsi attorno agli assi dello schermo
   come la `head_pose` dello Stream Engine: il pitch è l'elevazione della
   direzione in cui guarda il viso, **positivo verso l'alto**. Filtro 1€ sul
   pitch.
5. **Applicazione**: l'emitter scrive il pitch nei campioni gaze vicini
   (entro 150 ms dall'immagine), con il bit 7 dei flag UDP. Con
   `-headpose-rotation image` (default) sostituisce anche yaw e roll ricavati
   dagli occhi con quelli del viso; senza viso tracciato restano quelli degli
   occhi. I bit 6/7/8 dei flag v2 dicono quali angoli vengono dalle immagini.

Il modello **non va addestrato**: è la rete di MediaPipe, già addestrata da
Google, che funziona bene anche sulle immagini IR. Finché la camera non è
calibrata, il pitch resta non valido.

**Perché yaw e roll dalle immagini.** Il 4C ha una sola camera, sotto lo
schermo, e dei due centri corneali misura bene la posizione sul piano
dell'immagine ma non quale dei due è più vicino. Lo yaw, che dipende proprio
da quella differenza di profondità, esce dalla linea degli occhi sottostimato
di circa 5 volte, e la differenza di profondità diventa in parte roll. Misure
con una persona davanti (2026-11-04, `-stats 1s`):

| movimento | viso: yaw / roll | occhi: yaw / roll |
|---|---|---|
| testa dritta | +5° / −1° | 0° / 0° |
| girata a sinistra | +42° / −8° | +3…+7° / +10° |
| girata a destra | −31° / +2° | −8…−10° / −9° |
| inclinata a destra | −5° / −28° | −6° / −28° |

Con la testa girata il viso resta tracciato nel 100% delle immagini, mentre
oltre 25° circa il tracker perde gli occhi, e con loro lo sguardo e la
posizione. Per l'inclinazione pura gli occhi sono precisi quanto il viso.
Con la testa dritta lo yaw del viso ha uno scarto fisso di qualche grado,
che dipende dalla calibrazione della camera: le applicazioni lo tolgono con
il loro punto neutro.

**Calibrazione della camera.** Il driver abbina il centro di ciascuna iride
trovato dal modello al centro corneale 3D dato dal tracker, poi risolve
focale, punto principale, rotazione e posizione della camera con un
Levenberg-Marquardt robusto, provando anche l'ipotesi di un'immagine
speculare. Cose misurate sul 4C provato (sessioni del 2026-11-04):

- **Camera:** circa 6 cm verso la destra dell'utente rispetto all'origine del
  tracker; asse ottico parallelo all'asse Z del tracker (0,3–0,5°); focale di
  circa 690 px; immagine non speculare.
- **Ritardo:** l'immagine 0x501 ha un timestamp in ritardo di circa 25 ms
  rispetto ai campioni gaze che vedono gli stessi occhi, e il driver lo
  compensa.
- **Punto principale:** i dati raccolti davanti al tracker non distinguono
  uno spostamento del punto principale da una rotazione della camera. Con un
  prior largo il risolutore lo spostava di 260 px e ruotava la camera di 20°,
  e lo yaw dell'immagine differiva di 21° da quello degli occhi. Per questo il
  prior sul punto principale è stretto (5 px).
- **Calibrazione adottata:** errore di 2,8–3,2 px e incertezza di 0,3–0,7°.
  Con la testa dritta yaw e roll dell'immagine coincidono con quelli degli
  occhi entro 1–3°; con la testa girata no (vedi sopra).
- **Verifica del pitch:** +11° con il mento alzato e −21° con il mento
  abbassato.

**Controlli nel log** (`head pose`, ogni `-stats`): immagini ricevute ed
elaborate, percentuale con il viso, millisecondi per immagine, pitch, e
soprattutto `yaw_img_minus_eyes_deg` / `roll_img_minus_eyes_deg`, la
differenza tra yaw e roll visti dall'immagine e quelli ricavati dagli occhi.
Vicini a zero **con la testa dritta** vuol dire che camera, modello e display
area sono coerenti.

## Note sul protocollo

1. **Origine.** Gli installer per Linux esistenti per il 4C, come
   `Eitol/tobii_eye_tracker_linux_installer` e `johngebbie/tobii_4C_for_linux`,
   installano i binari proprietari Tobii (`tobiiusbservice`,
   `libtobii_stream_engine.so`). Questo driver non li usa: parla il protocollo
   ricostruito dai progetti clean-room per l'Eye Tracker 5
   [Tropaion/Tobii_Linux](https://github.com/Tropaion/Tobii_Linux) (wiki),
   [Krazubu/tobii-linux](https://github.com/Krazubu/tobii-linux) e
   [Aetherall/tobiifree](https://github.com/Aetherall/tobiifree), e ne ha
   verificato la validità sul 4C: stesso framing, stessi op, stesse 39
   colonne gaze (i nomi li restituisce il firmware stesso, vedi `tobii4c info`).
2. **Big-endian.** Header TTP e campi TLV sono big-endian. Le coordinate sono
   fixed-point `i64 / 2^42` (tipo `0x04`), non float. L'unico campo
   little-endian è la lunghezza nell'envelope USB. L'output UDP del driver
   invece è little-endian `float32`.
3. **Endpoint bulk, non interrupt**: `0x83` IN, `0x05` OUT. `0x04` OUT esiste
   ma nessuna implementazione nota lo usa, e qui non viene mai scritto. Le
   interfacce 1 e 2 sono una camera UVC 640×480 con formato vendor, non usata.
4. **Nessuno stream di head pose.** Il catalogo del firmware elenca solo
   `gaze`, `image`, `presence`, `image_collection`, `algodbg`, `log`. La posa
   della testa viene **derivata** dai centri corneali 3D: posizione (punto
   medio degli occhi, mm), yaw e roll (dalla linea interoculare). Il pitch non
   è osservabile da due punti e viene dalle immagini (vedi sopra). La posa è
   nel **sistema dello schermo**, come la `head_pose` dello Stream Engine: il
   firmware invia ogni centro corneale sia nel sistema del tracker
   (0x02/0x08) sia dal centro della display area (0x22/0x24, "Gaze Origin
   Display", il `gaze_origin` di Tobii). La trasformazione tracker → schermo
   è verificata a meno di 2 µm su una cattura ET5 (`ttp.DisplayArea.ToDisplay`).
5. **Persistenza.** A differenza dell'ET5, il 4C **non si riavvia** alla
   chiusura della sessione e non richiede autenticazione realm (query
   realm = 0). Però **non conserva display area e calibrazione senza
   alimentazione**: dopo un'accensione a freddo (osservato il 2026-11-05 e il
   2026-11-11) la display area è 4 × 4 mm, il valore di fabbrica, e il
   2026-11-09, dopo aver staccato e ricollegato il 4C, il blob di calibrazione
   letto era quello di fabbrica (1469 byte). Per questo il servizio le
   riscrive a ogni connessione.
6. **Nessun keep-alive di protocollo**: dopo il subscribe il dispositivo
   trasmette finché la sessione resta aperta. Il driver ha comunque un watchdog
   (stallo → resubscribe → riconnessione) e un ping di salute opzionale che
   misura l'RTT.
7. **Lo stream `presence` (0x504) è a eventi**: un messaggio alla
   sottoscrizione e poi solo ai cambi di stato. I valori sono quelli dell'enum
   dello Stream Engine: 1 = assente, 2 = presente, 0 = sconosciuto.
8. **La colonna 0x1d/0x1e è la validità del track box** ("Eye 3D Relative
   Position Validity"), non dello sguardo 2D del singolo occhio, che non ha
   una validità propria e segue lo stato di tracciamento dell'occhio.
9. **Lo stream `image` (0x501) è l'immagine intera della camera IR**: riga XDS
   da 307 350 byte a 30 Hz con timestamp (stesso orologio del gaze), 8 bit
   per pixel, 640 × 480, in scala di grigi. Sottoscriverlo non rallenta il
   gaze. `image_collection` (0x508) viene rifiutato (`UNAUTHORIZED`). La
   colonna "Eye 2D position relative to sensor area" (0x19/0x1a) vale −1 anche
   con l'utente tracciato, quindi per sapere dove sono gli occhi
   nell'immagine il driver usa il modello del viso.
10. **In sola lettura il firmware espone anche** (verificato il 2026-11-07) il
    track box (op 0x578, 8 vertici: 300 × 200 mm a 450 mm, 550 × 400 mm a
    900 mm), proprietà (op 0x532) e stato (op 0x5d2) come coppie chiave →
    testo, e lo stream `log` (0x1772), che si sottoscrive senza autenticazione
    e invia righe di testo del firmware. `algodbg` (0x1770) arriva a 90 Hz con
    un blob vuoto. `tobii4c info -probe` mostra le risposte grezze.
11. **Frequenza**: 90,0 Hz con l'utente davanti; senza nessuno davanti il 4C
    trasmette a circa 89,2 Hz (periodo di 11,23 ms sull'orologio del
    dispositivo, frame counter senza buchi; misura del 2026-11-11).
12. **L'errore `EyeCmdErr ... 0x42e` che compare in Talon** è l'op 1070
    `CALIBRATE_POINTS_APPLY`: un errore di calibrazione, non di
    inizializzazione.

In breve:

```
envelope 8 B (LE)        header TTP 24 B (BE)                         payload
[dir 0 0 0][len u32]     [marker][seq][status][op][error][plen]        00 00 + TLV [type u8][len u32 BE][value]
 dir 0x00 host→dev        marker 0x51 req, 0x52 resp, 0x53 stream, 0x4e notifica
 dir 0x01 dev→host        len OUT = byte dopo l'envelope; len IN = messaggio intero
```

Sessione: control transfer vendor `0x41` (open) e `0x42` (close) su
interfaccia 0. Handshake: `0x3e8` hello, `0x58c` identità, `0x640` realm,
`0x4b0` catalogo stream, `0x4ba` colonne gaze, `0x83e` mounting, `0x596`
display area, `0x4c4` subscribe `0x500`.

Calibrazione (sequenza dello Stream Engine Windows, catture di
Krazubu/tobii-linux): `1100` lettura del blob attivo (backup), `1010` start,
`1060` clear, poi per i batch {1, 3, 3} punti `1030` collect (x, y, maschera
occhi 3) e `1070` compute, infine `1100` lettura del nuovo blob e `1020` stop.
Un blob si riscrive con `1110`. I comandi più lunghi di 4087 byte partono in
più bulk OUT, ciascuna col proprio envelope.

## Architettura

```
          rawCh (cap 1)              jobs (cap 64)             results
┌────────┐ []byte crudi ┌────────┐ frame + seq  ┌──────────┐  Sample   ┌──────────┐
│ reader │─────────────→│ framer │─────────────→│ worker×N │──────────→│ emitter  │→ UDP / LSL / stdout
│ 1 gor. │← rawFree     │ 1 gor. │              │ TLV→XDS  │           │ riordino │
└────────┘              └────────┘              └──────────┘           └──────────┘
 bloccata solo su        riassembla i messaggi,  zero-alloc,           ordine per seq,
 USB IN 0x83 (16 URB     risposte → richieste    Row riusata           drop detection sul
 in coda nel kernel)     in attesa, stream → job                       frame counter, statistiche
                                                    frameFree ←──────────────┘ (buffer restituito dopo l'emissione)
```

- **reader**: l'unica goroutine che tocca l'endpoint IN. Tiene 16 transfer da
  64 KiB sempre in coda nel kernel (1 MiB, più di tre immagini IR da 300 KB),
  così il dispositivo trova buffer pronti anche mentre il frame precedente
  viene passato avanti.
- **framer**: riassembla i messaggi (frammentazione, accorpamento,
  risincronizzazione, risposte a pezzi), consegna le risposte alle richieste in
  attesa (match su `seq` e `op`), copia i frame gaze in buffer pre-allocati e
  assegna a ciascuno lo stato dello stream presence, nell'ordine di arrivo
  dall'USB. Se la pipeline è satura scarta il frame (`host_drops`) invece di
  accumulare latenza.
- **worker**: decodificano TLV → riga XDS → `gaze.Sample` (struct a dimensione
  fissa, passata per valore). Nessuna allocazione.
- **emitter**: ripristina l'ordine con un ring indicizzato per `seq`, rileva i
  buchi nel frame counter del dispositivo (`device_drops`) e scrive sui sink.
  Il buffer del frame torna al pool solo dopo l'emissione: così i frame in volo
  non superano mai la dimensione del ring.
- **head pose** (facoltativo): il framer consegna le immagini 0x501 a una
  goroutine separata senza mai bloccare (se è occupata l'immagine è
  scartata); l'emitter le passa ogni campione prima dei sink, per registrare
  gli occhi e applicare l'ultimo pitch stimato.
- **supervisor**: watchdog sullo stream, keep-alive, arresto ordinato
  (unsubscribe e chiusura sessione a pipeline ancora attiva), riconnessione con
  backoff esponenziale su hotplug.

A 90 Hz la decodifica occupa ~0,01% di un core: il pool di worker non riduce
la latenza, che dipende dal polling USB e dall'elaborazione nel dispositivo.

| Pacchetto | Contenuto |
|---|---|
| `internal/ttp` | framing, riassemblaggio, TLV, righe XDS, comandi, display area. Puro Go, testato su catture reali |
| `internal/gaze` | `Sample`, mappa chiavi → campi, posa della testa derivata, presenza |
| `internal/usbdev` | trasporto gousb: detach del driver kernel, claim, sessione vendor 0x41/0x42 |
| `internal/tracker` | pipeline concorrente, handshake, watchdog, riconnessione |
| `internal/sink` | UDP (T4CG v2), LSL (cgo, build tag `lsl`), stampa |
| `internal/calib` | logica di calibrazione: fissazione, raccolta, compute, backup e ripristino, metriche di accuratezza, simulatore |
| `internal/calibui` | interfaccia a schermo intero: menu, posizionamento del sensore, posizione, calibrazione, verifica, calibrazione della camera |
| `internal/x11ui` | finestra X11 in Go puro (`jezek/xgb`): RandR, EDID, tastiera, disegno con antialiasing, testo |
| `internal/irimg` | decodifica dello stream immagine 0x501 |
| `internal/nn` | interprete ONNX minimale in Go puro (Conv, depthwise, PRelu, Add, MaxPool, Pad, Reshape, Transpose, Sigmoid), kernel AVX2/FMA in assembly |
| `internal/facemodel` | il modello del viso di MediaPipe in ONNX, incorporato nel binario |
| `internal/headpose` | landmark del viso, fit del modello canonico, calibrazione della camera, pitch nel sistema dello schermo |
| `internal/store` | `~/.config/tobii4c`: calibrazione, geometria dello schermo, calibrazione della camera, backup |
| `cmd/tobii4c` | CLI |

## Sviluppo

```sh
make test     # go vet + go test: nessun test richiede il dispositivo
make bench    # decodifica di un frame gaze e inferenza del modello del viso
make race     # bin/tobii4c-race, con il race detector
go test -race ./internal/calib/   # flusso di calibrazione completo col simulatore
```

Commenti, messaggi di commit e documentazione sono in italiano; gli
identificatori Go e i messaggi di log restano in inglese. Le misure riportate
in questo README sono reali e datate: vanno cambiate solo dopo averle rifatte.

Cosa coprono i test:

- `internal/ttp`: catture USB reali dell'ET5 (MIT, vedi
  `internal/ttp/testdata/README.md`). Il frame gaze viene decodificato
  **bit per bit uguale** ai valori consegnati dallo Stream Engine Windows.
  Coprono anche il riassemblaggio a chunk da 1 a N byte, la
  risincronizzazione, le risposte frammentate e le 0 allocazioni.
  `DisplayArea.ToDisplay` riproduce le colonne "Gaze Origin Display" del
  firmware a partire da quelle del tracker. I comandi di calibrazione
  (collect, compute, read, start) sono **byte per byte identici** alle catture
  Windows, compreso un blob da 659 KB riassemblato da risposte a pezzi.
- `internal/gaze`: mappatura, invalidazione dell'occhio non tracciato, posa
  della testa nel sistema dello schermo, track box con la sua validità,
  apertura delle palpebre, messaggio presence reale dell'ET5.
- `internal/sink`: round-trip del pacchetto v2, offset dichiarati, lettura dei
  pacchetti v1, canali LSL (NaN per i dati non validi), consegna UDP, 0
  allocazioni.
- `internal/calib`: flusso completo col simulatore (sotto `-race`),
  annullamento con ripristino del backup, display area rifiutata a
  calibrazione aperta, occhi non rilevati, geometria del mounting del 4C.
- `internal/tracker`: assegnazione della presenza ai frame gaze con i messaggi
  reali dell'ET5 passati a framer e worker, inoltro delle immagini allo
  stimatore, chiamata di `Observe` prima dei sink.
- `internal/nn`: un grafo ONNX costruito nel test confrontato con
  un'implementazione di riferimento banale, con e senza AVX2 e con 1 e 3
  goroutine; il modello reale si carica.
- `internal/headpose`: Procrustes, angoli di Eulero e loro convenzioni,
  ritaglio; una testa sintetica in pose note proiettata con una camera nota
  (errore < 0,11°); calibrazione della camera da coppie sintetiche, con
  immagine diritta e speculare (errore di rotazione 0,25°); stimatore completo
  con un modello finto, che si calibra da solo in 13 s di movimento simulato e
  poi ha un errore massimo di 0,27° sul pitch. Con `TOBII4C_FACE_IMAGE` (PNG
  o JPEG) il modello vero gira su una foto.
- `cmd/tobii4c`: `-display-mm saved` e `-calibration saved` con e senza i file
  salvati.
- `internal/x11ui`: la cache dei glifi disegna pixel per pixel come il
  rasterizzatore.

Verifiche sul 4C reale:

- mounting (2 segni a 184 mm, 20°), lettura del blob (335 880 byte in 188 ms)
  e riscrittura con rilettura identica byte per byte; display area
  698 × 393 mm dal setup a frecce; tre calibrazioni complete; annullamenti con
  ripristino verificato dalla notifica 3220; verifiche fra 0,61° e 0,99°;
- pipeline: la tabella in [Stato](#stato);
- pitch: immagini a 30 Hz, gaze a 90 Hz con latenza p50 di 14–45 µs; con
  l'utente assente la ricerca del viso non parte; con una persona davanti
  (2026-11-04) viso trovato nel 100% delle immagini, circa 27 elaborate al
  secondo, pitch +11° / −21° con il mento alzato e abbassato;
- installazione da zero (2026-11-04) su una VM Debian 13.7 appena installata,
  senza desktop (VirtualBox 7.1, 4C in passthrough USB): build, test e
  benchmark, `go install`, udev (`uaccess` con un login locale, variante
  `plugdev` via SSH), LSL a 89,9 Hz, `calibrate` e `headpose-calibrate` su
  Xvfb, `calibration apply`/`restore` con il blob identico byte per byte,
  servizio systemd, hotplug con riconnessione, `camera.json` copiato da un
  altro computer. Con le immagini sempre attive: 2 minuti di gaze a 90 Hz
  senza frame persi e immagini IR a 30 Hz.

## Risoluzione dei problemi

| Sintomo | Causa e rimedio |
|---|---|
| `device or resource busy` | un altro processo tiene il tracker: `systemctl --user stop tobii4c`, oppure un altro `tobii4c` (`pgrep -a tobii4c`), un programma che lo avvia da sé, Talon o `tobiiusbservice` |
| `libusb: bad access` | regola udev non installata (`make install-udev`), oppure nessuna sessione locale attiva (solo SSH: vedi la variante `plugdev`) |
| `no Tobii tracker found` | il 4C non è collegato o non è enumerato: `lsusb -d 2104:0127` |
| `connessione a X11 ($DISPLAY)` | `calibrate` e `headpose-calibrate` vanno lanciati dalla sessione grafica; un servizio deve avere `DISPLAY` e `XAUTHORITY` (`systemctl --user import-environment DISPLAY XAUTHORITY`) |
| sguardo spostato o compresso dopo un riavvio | il 4C ha perso display area e calibrazione: il servizio le riscrive con `-display-mm saved -calibration saved`; per un `run` lanciato a mano aggiungere gli stessi flag. `tobii4c display` mostra la display area attuale |
| `no saved calibration` / `no saved display geometry` nel log | manca ancora `tobii4c calibrate` |
| pitch sempre non valido | camera non calibrata (`tobii4c headpose-calibrate`), viso non visibile nelle immagini IR, o `-headpose off` |

## Limiti noti

- Provato su un solo esemplare di 4C (firmware 4.7.0).
- **Pitch della testa**: verificato sul 4C con una persona, ma non confrontato
  con la head pose del software Tobii originale, che su Linux non c'è. Costa
  circa due terzi di un core a 30 inferenze al secondo (riducibile con
  `-headpose-rate`). Quando il 4C perde gli occhi abbassa gli illuminatori: il
  viso non si vede più e il pitch non è disponibile.
- **Apertura delle palpebre**: il firmware del 4C la invia sempre a −1 (come
  l'ET5), quindi su questo dispositivo non è disponibile.
- **Presenza**: sul 4C passa ad "assente" anche quando perde gli occhi per un
  momento (per esempio guardando in basso), non solo quando l'utente se ne va.
- **Con lo stream immagine attivo il 4C salta circa un frame gaze al minuto**
  (0,02%). Misure del 2026-11-04:

  | Configurazione | Durata | Frame persi |
  |---|---|---:|
  | senza immagini | 2 min | 0 |
  | immagini, senza stimatore, trasferimenti USB 16 KB × 8 | 8 min | 6 |
  | immagini, trasferimenti USB 64 KB × 16 (default attuale) | 8 min | 2 |
  | immagini e stimatore, trasferimenti USB 16 KB × 8 | 8 min | 6 |

  Il frame manca dal dispositivo, non dal PC: il log riporta il salto
  dell'orologio del tracker (`device_gap_us`, 22,2 ms, cioè un frame) e
  l'intervallo di arrivo sul PC (`host_gap_us`, 22,0–22,2 ms). Per ridurre
  l'esposizione il driver **sospende lo stream immagine dopo 3 s di assenza**
  dell'utente, quando le immagini sono comunque buie, e lo riprende appena
  l'utente torna; `-headpose-images-always` lo tiene sempre attivo.

  I trasferimenti in coda contano quando la latenza USB cresce, per esempio
  in una macchina virtuale. In una VM VirtualBox con il 4C in passthrough e
  le immagini sempre attive, i vecchi 16 KB × 8 perdevano l'**1,4%** dei frame
  gaze (51 su 3 522 in 40 s); con 64 KB × 16, il default, la VM ne ha persi 2
  in 2 minuti, come il PC.
- Non sono esportate alcune colonne dello stream gaze: punto di sguardo 3D per
  occhio (0x04/0x0a), centro del bulbo oculare (0x17/0x18), posizione 2D
  rispetto al sensore (0x19/0x1a), guida al posizionamento (0x25–0x28) e 0x11,
  senza nome. `tobii4c info` ne elenca i nomi.

## Licenza

MIT, vedi [`LICENSE`](LICENSE). I componenti di terze parti inclusi nel
repository sono il modello dei landmark del viso e i dati del modello canonico
del viso di MediaPipe (Apache 2.0) e le catture dell'Eye Tracker 5 di
Krazubu/tobii-linux (MIT). Sono elencati, insieme alle dipendenze, in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

Tobii è un marchio di Tobii AB. Questo è un progetto indipendente, non
affiliato, sponsorizzato né approvato da Tobii. Il driver scrive sul
dispositivo calibrazione e display area (della calibrazione fa prima un
backup): usalo a tuo rischio.
