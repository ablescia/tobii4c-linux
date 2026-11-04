#!/usr/bin/env python3
"""Ricevitore minimale dei datagrammi T4CG di tobii4c (solo libreria standard).

Legge la versione 2 (192 byte) e, per compatibilità, la versione 1 (120 byte).

uso: python3 examples/recv.py [host] [porta]     (default 127.0.0.1 5005)
"""
import math
import socket
import struct
import sys
import time

V1 = struct.Struct("<4sBBHQqq2f2f3f3f3f3fff2f2f")  # 120 byte, little-endian
V2 = struct.Struct("<4sBBHQqq2f2f3f3f3f3fff2f2fIB3xq3f3f3f3fff")  # 192 byte
assert V1.size == 120 and V2.size == 192

FLAG_GAZE, FLAG_HEAD_POS = 1 << 0, 1 << 6
FLAG2_OPEN_L, FLAG2_OPEN_R = 1 << 4, 1 << 5
PRESENCE = {0: "sconosciuta", 1: "assente", 2: "presente"}


def main() -> None:
    host = sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1"
    port = int(sys.argv[2]) if len(sys.argv) > 2 else 5005
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind((host, port))
    print(f"in ascolto su {host}:{port}", file=sys.stderr)
    last_print = 0.0
    while True:
        data, _ = sock.recvfrom(2048)
        if len(data) < V1.size or data[:4] != b"T4CG":
            continue
        ver = data[4]
        if ver == 2 and len(data) >= V2.size:
            f = V2.unpack_from(data)
        elif ver == 1:
            f = V1.unpack_from(data)
        else:
            continue
        _, _, _, flags, seq, host_ns, _ = f[:7]
        gx, gy = f[7], f[8]
        hx, hy, hz = f[11:14]
        yaw, roll = f[15], f[16]
        now = time.time()
        if now - last_print < 0.25:
            continue
        last_print = now
        age_us = (time.time_ns() - host_ns) / 1e3
        gaze = f"({gx:.3f},{gy:.3f})" if flags & FLAG_GAZE else "   --   "
        head = (f"({hx:.0f},{hy:.0f},{hz:.0f})mm yaw={math.degrees(yaw):+.1f}° roll={math.degrees(roll):+.1f}°"
                if flags & FLAG_HEAD_POS else "--")
        extra = ""
        if ver == 2:
            flags2, presence = f[29], f[30]
            open_l, open_r = f[44], f[45]
            ol = f"{open_l:.1f}" if flags2 & FLAG2_OPEN_L else "--"
            orr = f"{open_r:.1f}" if flags2 & FLAG2_OPEN_R else "--"
            extra = f" apertura={ol}/{orr}mm presenza={PRESENCE.get(presence, presence)}"
        print(f"#{seq} v{ver} gaze={gaze} head={head}{extra} età={age_us:.0f}µs", flush=True)


if __name__ == "__main__":
    main()
