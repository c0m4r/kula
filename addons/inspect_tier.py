#!/usr/bin/env python3
"""
Utility to inspect Kula tier files (v1 JSON and v2 binary codec).

v2 binary layout mirrors internal/storage/codec.go exactly.
"""

import struct
import sys
import os
import datetime
import json
import math
import mmap
import re
import zlib
from typing import Optional, Tuple, Dict, Any, Iterator, List

MAGIC = b"KULA"
HEADER_SIZE = 64
CODEC_V2 = 2

# Header flags packed into bytes [4:8]. Absent (zero) in pre-tail-tracking files.
FLAG_HAS_TAIL = 1 << 0  # header carries a valid oldest-record offset
FLAG_WRAPPED = 1 << 1  # ring has wrapped; an old segment exists

# Flag bits (preamble flags uint16)
FLAG_HAS_MIN = 1 << 0
FLAG_HAS_MAX = 1 << 1
FLAG_HAS_DATA = 1 << 2
FLAG_HAS_APPS = 1 << 3
FLAG_REDUCER_V2 = 1 << 4
FLAG_HAS_MEAN_STATS = 1 << 11
FLAG_HAS_APACHE2 = 1 << 8
FLAG_HAS_MYSQL = 1 << 9
FLAG_HAS_PSU = 1 << 10
FLAG_HAS_DISK_IDS = 1 << 12
FLAG_HAS_TCP_EXT = 1 << 13
TCP_EXT_VERSION = 1
TCP_EXT_V1_SIZE = 4  # retrans_ps float32

FIXED_BLOCK_SIZE = 218  # must match fixedBlockSize in codec.go

# Record kind bytes (first payload byte; see codec.go and record_checksum.go).
RECORD_KIND_BINARY = 0x02  # written before 0.22.0, no checksum
RECORD_KIND_CHECKSUMMED = 0x03  # [0x03][encoded sample][crc32 u32 LE]
RECORD_CHECKSUM_SIZE = 4

# Ring walking, as Tier.walkSegment and resyncToNextRecord in tier.go do it.
MAX_RECORD_BYTES = 64 << 20  # maxRecordBytes
RESYNC_PEEK = 4096  # resyncPeek: window for the two-records-in-a-row check
SCAN_BUFFER = 1 << 20  # the scan's bufio size; resync verifies no larger record
# Resync rejects timestamps before 2000 (local time, as Go's Year() reads it)
# and more than an hour past the header's newest sample.
MIN_SANE_NS = int(datetime.datetime(2000, 1, 1).timestamp()) * 10**9
MAX_AHEAD_NS = 3600 * 10**9
_NONZERO = re.compile(rb"[^\x00]")


# ---------------------------------------------------------------------------
# Header
# ---------------------------------------------------------------------------


def parse_header(buf: bytes) -> Tuple[bytes, int, int, int, int, int, int, int, int]:
    """Parse a 64-byte tier header.

    Returns: magic, flags, codec_ver, max_data, write_off, count,
    oldest_nano, newest_nano, oldest_off.
    """
    # magic[4] flags[4] version[8] maxData[8] writeOff[8] count[8]
    # oldestNano[8] newestNano[8] oldestOff[8]
    return struct.unpack("<4sIQQQQqqQ", buf)


# ---------------------------------------------------------------------------
# v2 binary helpers
# ---------------------------------------------------------------------------


def _get_u8(data: bytes, off: int) -> Tuple[int, int]:
    return data[off], off + 1


def _get_u16(data: bytes, off: int) -> Tuple[int, int]:
    return struct.unpack_from("<H", data, off)[0], off + 2


def _get_i32(data: bytes, off: int) -> Tuple[int, int]:
    return struct.unpack_from("<i", data, off)[0], off + 4


def _get_u32(data: bytes, off: int) -> Tuple[int, int]:
    return struct.unpack_from("<I", data, off)[0], off + 4


def _get_u64(data: bytes, off: int) -> Tuple[int, int]:
    return struct.unpack_from("<Q", data, off)[0], off + 8


def _get_f32(data: bytes, off: int) -> Tuple[float, int]:
    v = struct.unpack_from("<f", data, off)[0]
    return round(float(v), 6), off + 4


def _get_i64(data: bytes, off: int) -> Tuple[int, int]:
    return struct.unpack_from("<q", data, off)[0], off + 8


def _get_f64(data: bytes, off: int) -> Tuple[float, int]:
    v = struct.unpack_from("<d", data, off)[0]
    return v, off + 8


def _get_str(data: bytes, off: int) -> Tuple[str, int]:
    """uint8-length-prefixed UTF-8 string (matches appendStr / getStr in Go)."""
    if off >= len(data):
        return "", off
    n = data[off]
    off += 1
    end = min(off + n, len(data))
    return data[off:end].decode("utf-8", errors="replace"), end


# ---------------------------------------------------------------------------
# Fixed block decoder — mirrors decodeFixed() in codec.go
#
# Offsets match appendFixed() exactly:
#   [0:28]    cpu total × 7 float32 (usage,user,sys,iowait,irq,softirq,steal)
#   [28:30]   num_cores uint16
#   [30:34]   cpu_temp  float32
#   [34:46]   load[3]   float32  (load1, load5, load15)
#   [46:48]   load_running uint16
#   [48:50]   load_total   uint16
#   [50:106]  mem[7]    uint64   (total,free,avail,used,buffers,cached,shmem)
#   [106:110] mem_used_pct float32
#   [110:134] swap[3]   uint64   (total,free,used)
#   [134:138] swap_used_pct float32
#   [138:146] tcp_curr_estab uint64
#   [146:150] tcp_in_errs float32
#   [150:154] tcp_out_rsts float32
#   [154:158] sock_tcp_inuse int32
#   [158:162] sock_tcp_tw    int32
#   [162:166] sock_udp_inuse int32
#   [166:190] proc[6] int32 (total,running,sleeping,zombie,blocked,threads)
#   [190:198] uptime_sec float64  (NOT float32!)
#   [198:202] entropy    int32
#   [202:203] user_count uint8 (255 = unknown, decoded as -1)
#   [203:204] clock_sync uint8
#   [204:212] self_rss   uint64
#   [212:216] self_cpu_pct float32
#   [216:218] self_fds  uint16
# ---------------------------------------------------------------------------


def _decode_fixed(data: bytes, off: int) -> Tuple[Dict[str, Any], int]:
    # pylint: disable=too-many-locals
    if off + FIXED_BLOCK_SIZE > len(data):
        return {}, off + FIXED_BLOCK_SIZE

    base = off

    # CPU total (0..28)
    cpu_usage, off = _get_f32(data, off)
    cpu_user, off = _get_f32(data, off)
    cpu_sys, off = _get_f32(data, off)
    cpu_iowait, off = _get_f32(data, off)
    cpu_irq, off = _get_f32(data, off)
    cpu_softirq, off = _get_f32(data, off)
    cpu_steal, off = _get_f32(data, off)

    # CPU meta (28..34)
    num_cores, off = _get_u16(data, off)
    cpu_temp, off = _get_f32(data, off)

    # Load average (34..50)
    load1, off = _get_f32(data, off)
    load5, off = _get_f32(data, off)
    load15, off = _get_f32(data, off)
    running, off = _get_u16(data, off)
    total, off = _get_u16(data, off)

    # Memory (50..110)
    mem_total, off = _get_u64(data, off)
    mem_free, off = _get_u64(data, off)
    mem_avail, off = _get_u64(data, off)
    mem_used, off = _get_u64(data, off)
    mem_buffers, off = _get_u64(data, off)
    mem_cached, off = _get_u64(data, off)
    mem_shmem, off = _get_u64(data, off)
    mem_pct, off = _get_f32(data, off)

    # Swap (110..138)
    swap_total, off = _get_u64(data, off)
    swap_free, off = _get_u64(data, off)
    swap_used, off = _get_u64(data, off)
    swap_pct, off = _get_f32(data, off)

    # TCP + sockets (138..166)
    tcp_estab, off = _get_u64(data, off)
    tcp_inerrs, off = _get_f32(data, off)
    tcp_outrst, off = _get_f32(data, off)
    sock_tcp, off = _get_i32(data, off)
    sock_tw, off = _get_i32(data, off)
    sock_udp, off = _get_i32(data, off)

    # Process (166..190)
    proc_total, off = _get_i32(data, off)
    proc_running, off = _get_i32(data, off)
    proc_sleep, off = _get_i32(data, off)
    proc_zombie, off = _get_i32(data, off)
    proc_blocked, off = _get_i32(data, off)
    proc_threads, off = _get_i32(data, off)

    # System (190..204)  — note: uptime is float64!
    uptime, off = _get_f64(data, off)
    entropy, off = _get_i32(data, off)
    user_count, off = _get_u8(data, off)
    clock_sync, off = _get_u8(data, off)

    # Self (204..218)
    self_rss, off = _get_u64(data, off)
    self_cpu, off = _get_f32(data, off)
    self_fds, off = _get_u16(data, off)

    assert (
        off == base + FIXED_BLOCK_SIZE
    ), f"fixed decode mismatch: {off - base} != {FIXED_BLOCK_SIZE}"

    return {
        "cpu": {
            "usage": cpu_usage,
            "user": cpu_user,
            "system": cpu_sys,
            "iowait": cpu_iowait,
            "irq": cpu_irq,
            "softirq": cpu_softirq,
            "steal": cpu_steal,
            "num_cores": num_cores,
            "temp": cpu_temp,
        },
        "load": {
            "load1": load1,
            "load5": load5,
            "load15": load15,
            "running": running,
            "total": total,
        },
        "memory": {
            "total": mem_total,
            "free": mem_free,
            "available": mem_avail,
            "used": mem_used,
            "buffers": mem_buffers,
            "cached": mem_cached,
            "shmem": mem_shmem,
            "used_pct": mem_pct,
        },
        "swap": {
            "total": swap_total,
            "free": swap_free,
            "used": swap_used,
            "used_pct": swap_pct,
        },
        "tcp": {
            "curr_estab": tcp_estab,
            "in_errs_ps": tcp_inerrs,
            "out_rsts_ps": tcp_outrst,
            # Not in the fixed block: set by the TCP extension (section 10)
            # and 0 for records written before it, as in the Go decoder.
            "retrans_ps": 0.0,
        },
        "sockets": {"tcp_inuse": sock_tcp, "tcp_tw": sock_tw, "udp_inuse": sock_udp},
        "process": {
            "total": proc_total,
            "running": proc_running,
            "sleeping": proc_sleep,
            "zombie": proc_zombie,
            "blocked": proc_blocked,
            "threads": proc_threads,
        },
        "system": {
            "uptime_sec": uptime,
            "entropy": entropy,
            "user_count": -1 if user_count == 255 else user_count,
            "clock_synced": bool(clock_sync),
        },
        "self": {"cpu_pct": self_cpu, "mem_rss": self_rss, "fds": self_fds},
    }, off


# ---------------------------------------------------------------------------
# Variable block decoder — mirrors decodeVariable() in codec.go
#
# Section order (matches appendVariable):
#   1. Network interfaces:  uint16 count + per-iface entries
#   2. CPU sensors:         uint16 count + per-sensor entries
#   3. Disk devices:        uint16 count + per-device entries (with sub-sensors)
#   4. Filesystems:         uint16 count + per-fs entries
#   5. System strings:      hostname (str8), clock_source (str8)
#   6. GPU entries:         uint16 count + per-GPU entries
#   7. Application metrics: nginx (1B+52B), containers (u16+var),
#                           postgres (1B version + 56B or 104B), custom (u16 groups+var)
#   8. Power supplies:      1B version + u16 count + per-supply entries
#   9. Disk identities:     1B version + u16 count + per-disk u16-prefixed ID
#  10. TCP extension:       1B version + 4B v1 block (retrans_ps float32)
# ---------------------------------------------------------------------------


def _decode_variable(
    data: bytes,
    off: int,
    s: Dict[str, Any],
    has_apps: bool = False,
    has_apache2: bool = False,
    has_mysql: bool = False,
    has_psu: bool = False,
    has_disk_ids: bool = False,
    has_tcp_ext: bool = False,
) -> Tuple[Dict[str, Any], int]:
    # pylint: disable=too-many-locals, too-many-statements
    # 1. Network interfaces
    num_ifaces, off = _get_u16(data, off)
    ifaces = []
    for _ in range(num_ifaces):
        name, off = _get_str(data, off)
        rx_mbps, off = _get_f32(data, off)
        tx_mbps, off = _get_f32(data, off)
        rx_pps, off = _get_f32(data, off)
        tx_pps, off = _get_f32(data, off)
        rx_bytes, off = _get_u64(data, off)
        tx_bytes, off = _get_u64(data, off)
        rx_pkts, off = _get_u64(data, off)
        tx_pkts, off = _get_u64(data, off)
        rx_errs, off = _get_u64(data, off)
        tx_errs, off = _get_u64(data, off)
        rx_drop, off = _get_u64(data, off)
        tx_drop, off = _get_u64(data, off)
        ifaces.append(
            {
                "name": name,
                "rx_mbps": rx_mbps,
                "tx_mbps": tx_mbps,
                "rx_pps": rx_pps,
                "tx_pps": tx_pps,
                "rx_bytes": rx_bytes,
                "tx_bytes": tx_bytes,
                "rx_pkts": rx_pkts,
                "tx_pkts": tx_pkts,
                "rx_errs": rx_errs,
                "tx_errs": tx_errs,
                "rx_drop": rx_drop,
                "tx_drop": tx_drop,
            }
        )
    s.setdefault("network", {})["ifaces"] = ifaces

    # 2. CPU sensors
    num_sensors, off = _get_u16(data, off)
    sensors = []
    for _ in range(num_sensors):
        sname, off = _get_str(data, off)
        sval, off = _get_f32(data, off)
        sensors.append({"name": sname, "value": sval})
    s.setdefault("cpu", {})["sensors"] = sensors

    # 3. Disk devices (each has nested sub-sensor list)
    num_devs, off = _get_u16(data, off)
    devs = []
    for _ in range(num_devs):
        dname, off = _get_str(data, off)
        reads_ps, off = _get_f32(data, off)
        writes_ps, off = _get_f32(data, off)
        read_bps, off = _get_f32(data, off)
        write_bps, off = _get_f32(data, off)
        util_pct, off = _get_f32(data, off)
        temp, off = _get_f32(data, off)
        num_ds, off = _get_u16(data, off)  # nested DiskTempSensor count
        dsensors = []
        for _ in range(num_ds):
            dsname, off = _get_str(data, off)
            dsval, off = _get_f32(data, off)
            dsensors.append({"name": dsname, "value": dsval})
        devs.append(
            {
                "name": dname,
                "reads_ps": reads_ps,
                "writes_ps": writes_ps,
                "read_bps": read_bps,
                "write_bps": write_bps,
                "util_pct": util_pct,
                "temp": temp,
                "sensors": dsensors,
            }
        )
    s.setdefault("disks", {})["devices"] = devs

    # 4. Filesystems
    num_fs, off = _get_u16(data, off)
    filesystems = []
    for _ in range(num_fs):
        dev_name, off = _get_str(data, off)
        mountpoint, off = _get_str(data, off)
        fstype, off = _get_str(data, off)
        fs_total, off = _get_u64(data, off)
        fs_used, off = _get_u64(data, off)
        fs_avail, off = _get_u64(data, off)
        fs_pct, off = _get_f32(data, off)
        filesystems.append(
            {
                "device": dev_name,
                "mountpoint": mountpoint,
                "fstype": fstype,
                "total": fs_total,
                "used": fs_used,
                "available": fs_avail,
                "used_pct": fs_pct,
            }
        )
    s.setdefault("disks", {})["filesystems"] = filesystems

    # 5. System strings
    hostname, off = _get_str(data, off)
    clock_source, off = _get_str(data, off)
    s.setdefault("system", {})["hostname"] = hostname
    s.setdefault("system", {})["clock_source"] = clock_source

    # 6. GPU entries
    num_gpus, off = _get_u16(data, off)
    gpus = []
    for _ in range(num_gpus):
        idx, off = _get_u16(data, off)
        gname, off = _get_str(data, off)
        driver, off = _get_str(data, off)
        gtemp, off = _get_f32(data, off)
        vram_used, off = _get_u64(data, off)
        vram_total, off = _get_u64(data, off)
        vram_pct, off = _get_f32(data, off)
        load_pct, off = _get_f32(data, off)
        power_w, off = _get_f32(data, off)
        gpus.append(
            {
                "index": idx,
                "name": gname,
                "driver": driver,
                "temp": gtemp,
                "vram_used": vram_used,
                "vram_total": vram_total,
                "vram_pct": vram_pct,
                "load_pct": load_pct,
                "power_w": power_w,
            }
        )
    if gpus:
        s["gpu"] = gpus

    # 7. Application metrics (only present when flagHasApps is set in preamble)
    # A record without flagHasApps predates flagHasPSU too (appendPreamble only
    # ever sets the two together), so nothing follows section 6 there.
    if not has_apps:
        return s, off

    apps: Dict[str, Any] = {}

    # 7a. Nginx (1-byte presence + 52-byte fixed block)
    nginx_present, off = _get_u8(data, off)
    if nginx_present != 0:
        active_conn, off = _get_i32(data, off)
        accepts, off = _get_u64(data, off)
        handled, off = _get_u64(data, off)
        requests, off = _get_u64(data, off)
        accepts_ps, off = _get_f32(data, off)
        handled_ps, off = _get_f32(data, off)
        requests_ps, off = _get_f32(data, off)
        reading, off = _get_i32(data, off)
        writing, off = _get_i32(data, off)
        waiting, off = _get_i32(data, off)
        apps["nginx"] = {
            "active_connections": active_conn,
            "accepts": accepts,
            "handled": handled,
            "requests": requests,
            "accepts_ps": accepts_ps,
            "handled_ps": handled_ps,
            "requests_ps": requests_ps,
            "reading": reading,
            "writing": writing,
            "waiting": waiting,
        }

    # 7b. Containers (uint16 count + variable per container)
    num_containers, off = _get_u16(data, off)
    containers = []
    for _ in range(num_containers):
        ct_id, off = _get_str(data, off)
        ct_name, off = _get_str(data, off)
        cpu_pct, off = _get_f32(data, off)
        mem_used, off = _get_u64(data, off)
        mem_limit, off = _get_u64(data, off)
        mem_pct, off = _get_f32(data, off)
        net_rx_bps, off = _get_f32(data, off)
        net_tx_bps, off = _get_f32(data, off)
        disk_r_bps, off = _get_f32(data, off)
        disk_w_bps, off = _get_f32(data, off)
        containers.append(
            {
                "id": ct_id,
                "name": ct_name,
                "cpu_pct": cpu_pct,
                "mem_used": mem_used,
                "mem_limit": mem_limit,
                "mem_pct": mem_pct,
                "net_rx_bps": net_rx_bps,
                "net_tx_bps": net_tx_bps,
                "disk_r_bps": disk_r_bps,
                "disk_w_bps": disk_w_bps,
            }
        )
    if containers:
        apps["containers"] = containers

    # 7c. PostgreSQL — version-tagged presence byte:
    #   0 = not present
    #   1 = v1 (56-byte block: 3×i32 + 7×f32 + 2×i64)
    #   2 = v2 (104-byte block: 5×i32 + 13×f32 + 4×i64)
    #   3 = v3 (121-byte block: v2 + replication state)
    pg_version, off = _get_u8(data, off)
    if pg_version > 3:
        raise ValueError(f"unsupported postgres section version {pg_version}")
    if pg_version == 1:
        active_conns, off = _get_i32(data, off)
        idle_conns, off = _get_i32(data, off)
        max_conns, off = _get_i32(data, off)
        tx_commit_ps, off = _get_f32(data, off)
        tx_rollback_ps, off = _get_f32(data, off)
        tup_fetched_ps, off = _get_f32(data, off)
        tup_inserted_ps, off = _get_f32(data, off)
        tup_updated_ps, off = _get_f32(data, off)
        tup_deleted_ps, off = _get_f32(data, off)
        blks_hit_pct, off = _get_f32(data, off)
        dead_tuples, off = _get_i64(data, off)
        db_size_bytes, off = _get_i64(data, off)
        apps["postgres"] = {
            "version": 1,
            "active_conns": active_conns,
            "idle_conns": idle_conns,
            "max_conns": max_conns,
            "tx_commit_ps": tx_commit_ps,
            "tx_rollback_ps": tx_rollback_ps,
            "tup_fetched_ps": tup_fetched_ps,
            "tup_inserted_ps": tup_inserted_ps,
            "tup_updated_ps": tup_updated_ps,
            "tup_deleted_ps": tup_deleted_ps,
            "blks_hit_pct": blks_hit_pct,
            "dead_tuples": dead_tuples,
            "db_size_bytes": db_size_bytes,
        }
    elif pg_version >= 2:
        active_conns, off = _get_i32(data, off)
        idle_conns, off = _get_i32(data, off)
        idle_in_tx_conns, off = _get_i32(data, off)
        waiting_conns, off = _get_i32(data, off)
        max_conns, off = _get_i32(data, off)
        tx_commit_ps, off = _get_f32(data, off)
        tx_rollback_ps, off = _get_f32(data, off)
        tup_fetched_ps, off = _get_f32(data, off)
        tup_returned_ps, off = _get_f32(data, off)
        tup_inserted_ps, off = _get_f32(data, off)
        tup_updated_ps, off = _get_f32(data, off)
        tup_deleted_ps, off = _get_f32(data, off)
        blks_read_ps, off = _get_f32(data, off)
        blks_hit_ps, off = _get_f32(data, off)
        blks_hit_pct, off = _get_f32(data, off)
        deadlocks_ps, off = _get_f32(data, off)
        buf_checkpoint_ps, off = _get_f32(data, off)
        buf_backend_ps, off = _get_f32(data, off)
        dead_tuples, off = _get_i64(data, off)
        live_tuples, off = _get_i64(data, off)
        autovacuum_count, off = _get_i64(data, off)
        db_size_bytes, off = _get_i64(data, off)
        replica_count = 0
        is_in_recovery = False
        repl_lag_bytes = 0
        repl_lag_seconds = 0.0
        if pg_version >= 3:
            replica_count, off = _get_i32(data, off)
            in_rec_byte, off = _get_u8(data, off)
            is_in_recovery = in_rec_byte != 0
            repl_lag_bytes, off = _get_i64(data, off)
            repl_lag_seconds, off = _get_f32(data, off)
        apps["postgres"] = {
            "version": pg_version,
            "active_conns": active_conns,
            "idle_conns": idle_conns,
            "idle_in_tx_conns": idle_in_tx_conns,
            "waiting_conns": waiting_conns,
            "max_conns": max_conns,
            "tx_commit_ps": tx_commit_ps,
            "tx_rollback_ps": tx_rollback_ps,
            "tup_fetched_ps": tup_fetched_ps,
            "tup_returned_ps": tup_returned_ps,
            "tup_inserted_ps": tup_inserted_ps,
            "tup_updated_ps": tup_updated_ps,
            "tup_deleted_ps": tup_deleted_ps,
            "blks_read_ps": blks_read_ps,
            "blks_hit_ps": blks_hit_ps,
            "blks_hit_pct": blks_hit_pct,
            "deadlocks_ps": deadlocks_ps,
            "buf_checkpoint_ps": buf_checkpoint_ps,
            "buf_backend_ps": buf_backend_ps,
            "dead_tuples": dead_tuples,
            "live_tuples": live_tuples,
            "autovacuum_count": autovacuum_count,
            "db_size_bytes": db_size_bytes,
            "is_in_recovery": is_in_recovery,
            "replica_count": replica_count,
            "repl_lag_bytes": repl_lag_bytes,
            "repl_lag_seconds": repl_lag_seconds,
        }

    # 7d. MySQL — gated by has_mysql flag so old records skip this section.
    # Presence byte doubles as version tag:
    #   0 = not present
    #   1 = v1 format (56-byte block: 4×i32 + 10×f32)
    #   2 = v2 format (66-byte block: v1 + replication state)
    #   3 = v3 format (v2 + 8B errno + length-prefixed IOState string)
    if has_mysql:
        my_version, off = _get_u8(data, off)
        if my_version > 3:
            raise ValueError(f"unsupported mysql section version {my_version}")
        if my_version >= 1:
            threads_connected, off = _get_i32(data, off)
            threads_running, off = _get_i32(data, off)
            threads_cached, off = _get_i32(data, off)
            max_connections, off = _get_i32(data, off)
            queries_ps, off = _get_f32(data, off)
            com_select_ps, off = _get_f32(data, off)
            com_insert_ps, off = _get_f32(data, off)
            com_update_ps, off = _get_f32(data, off)
            com_delete_ps, off = _get_f32(data, off)
            slow_queries_ps, off = _get_f32(data, off)
            innodb_bp_hit_pct, off = _get_f32(data, off)
            innodb_bp_reads_ps, off = _get_f32(data, off)
            table_locks_waited_ps, off = _get_f32(data, off)
            row_lock_waits_ps, off = _get_f32(data, off)
            replica_count = 0
            replica_io_running = False
            replica_sql_running = False
            replica_seconds_behind = -1
            last_io_errno = 0
            last_sql_errno = 0
            io_state = ""
            if my_version >= 2:
                replica_count, off = _get_i32(data, off)
                io_byte, off = _get_u8(data, off)
                sql_byte, off = _get_u8(data, off)
                replica_io_running = io_byte != 0
                replica_sql_running = sql_byte != 0
                replica_seconds_behind, off = _get_i32(data, off)
            if my_version >= 3:
                last_io_errno, off = _get_i32(data, off)
                last_sql_errno, off = _get_i32(data, off)
                io_state, off = _get_str(data, off)
            apps["mysql"] = {
                "version": my_version,
                "threads_connected": threads_connected,
                "threads_running": threads_running,
                "threads_cached": threads_cached,
                "max_connections": max_connections,
                "queries_ps": queries_ps,
                "com_select_ps": com_select_ps,
                "com_insert_ps": com_insert_ps,
                "com_update_ps": com_update_ps,
                "com_delete_ps": com_delete_ps,
                "slow_queries_ps": slow_queries_ps,
                "innodb_buffer_pool_hit_pct": innodb_bp_hit_pct,
                "innodb_bp_reads_ps": innodb_bp_reads_ps,
                "table_locks_waited_ps": table_locks_waited_ps,
                "row_lock_waits_ps": row_lock_waits_ps,
                "replica_io_running": replica_io_running,
                "replica_sql_running": replica_sql_running,
                "replica_seconds_behind": replica_seconds_behind,
                "replica_count": replica_count,
                "replica_last_io_errno": last_io_errno,
                "replica_last_sql_errno": last_sql_errno,
                "replica_io_state": io_state,
            }

    # 7e. Apache2 — gated by has_apache2 flag so old records skip this section.
    # Presence byte doubles as version tag:
    #   0 = not present
    #   1 = v1 format (72-byte block)
    #   2 = v2 format (100-byte block: adds 7 scoreboard states)
    if has_apache2:
        apache2_ver, off = _get_u8(data, off)
        if apache2_ver == 1:
            busy_workers, off = _get_i32(data, off)
            idle_workers, off = _get_i32(data, off)
            total_accesses, off = _get_u64(data, off)
            total_kbytes, off = _get_u64(data, off)
            accesses_ps, off = _get_f32(data, off)
            kbytes_ps, off = _get_f32(data, off)
            req_per_sec, off = _get_f32(data, off)
            bytes_per_sec, off = _get_f32(data, off)
            bytes_per_req, off = _get_f32(data, off)
            cpu_load, off = _get_f32(data, off)
            uptime, off = _get_i64(data, off)
            waiting, off = _get_i32(data, off)
            reading, off = _get_i32(data, off)
            sending, off = _get_i32(data, off)
            keepalive, off = _get_i32(data, off)
            apps["apache2"] = {
                "busy_workers": busy_workers,
                "idle_workers": idle_workers,
                "total_accesses": total_accesses,
                "total_kbytes": total_kbytes,
                "accesses_ps": accesses_ps,
                "kbytes_ps": kbytes_ps,
                "req_per_sec": req_per_sec,
                "bytes_per_sec": bytes_per_sec,
                "bytes_per_req": bytes_per_req,
                "cpu_load": cpu_load,
                "uptime": uptime,
                "waiting": waiting,
                "reading": reading,
                "sending": sending,
                "keepalive": keepalive,
                "_format": "v1",
            }
        elif apache2_ver == 2:
            busy_workers, off = _get_i32(data, off)
            idle_workers, off = _get_i32(data, off)
            total_accesses, off = _get_u64(data, off)
            total_kbytes, off = _get_u64(data, off)
            accesses_ps, off = _get_f32(data, off)
            kbytes_ps, off = _get_f32(data, off)
            req_per_sec, off = _get_f32(data, off)
            bytes_per_sec, off = _get_f32(data, off)
            bytes_per_req, off = _get_f32(data, off)
            cpu_load, off = _get_f32(data, off)
            uptime, off = _get_i64(data, off)
            waiting, off = _get_i32(data, off)
            reading, off = _get_i32(data, off)
            sending, off = _get_i32(data, off)
            keepalive, off = _get_i32(data, off)
            starting, off = _get_i32(data, off)
            dns, off = _get_i32(data, off)
            closing, off = _get_i32(data, off)
            logging, off = _get_i32(data, off)
            graceful, off = _get_i32(data, off)
            idle_cleanup, off = _get_i32(data, off)
            open_slots, off = _get_i32(data, off)
            apps["apache2"] = {
                "busy_workers": busy_workers,
                "idle_workers": idle_workers,
                "total_accesses": total_accesses,
                "total_kbytes": total_kbytes,
                "accesses_ps": accesses_ps,
                "kbytes_ps": kbytes_ps,
                "req_per_sec": req_per_sec,
                "bytes_per_sec": bytes_per_sec,
                "bytes_per_req": bytes_per_req,
                "cpu_load": cpu_load,
                "uptime": uptime,
                "waiting": waiting,
                "reading": reading,
                "sending": sending,
                "keepalive": keepalive,
                "starting": starting,
                "dns": dns,
                "closing": closing,
                "logging": logging,
                "graceful": graceful,
                "idle_cleanup": idle_cleanup,
                "open_slots": open_slots,
                "_format": "v2",
            }
        elif apache2_ver != 0:
            raise ValueError(f"unsupported apache2 section version {apache2_ver}")

    # 7f. Custom metrics (uint16 group count, per group: str + uint16 metric count)
    num_groups, off = _get_u16(data, off)
    custom: Dict[str, List[Dict[str, Any]]] = {}
    for _ in range(num_groups):
        group_name, off = _get_str(data, off)
        metric_count, off = _get_u16(data, off)
        metrics = []
        for _ in range(metric_count):
            m_name, off = _get_str(data, off)
            m_value, off = _get_f32(data, off)
            metrics.append({"name": m_name, "value": m_value})
        custom[group_name] = metrics
    if custom:
        apps["custom"] = custom

    if apps:
        s["apps"] = apps

    # 8. Power supplies (batteries / UPS / mains) — gated by flagHasPSU.
    if has_psu:
        psu_ver, off = _get_u8(data, off)
        if psu_ver != 1:
            raise ValueError(f"unsupported psu section version {psu_ver}")
        num_psu, off = _get_u16(data, off)
        supplies = []
        for _ in range(num_psu):
            ps_name, off = _get_str(data, off)
            ps_type, off = _get_str(data, off)
            ps_status, off = _get_str(data, off)
            capacity, off = _get_u8(data, off)
            voltage_v, off = _get_f32(data, off)
            current_a, off = _get_f32(data, off)
            power_w, off = _get_f32(data, off)
            energy_now, off = _get_f32(data, off)
            energy_full, off = _get_f32(data, off)
            supplies.append(
                {
                    "name": ps_name,
                    "type": ps_type,
                    "status": ps_status,
                    "capacity": capacity,
                    "voltage_v": voltage_v,
                    "current_a": current_a,
                    "power_w": power_w,
                    "energy_wh_now": energy_now,
                    "energy_wh_full": energy_full,
                }
            )
        if supplies:
            s["psu"] = supplies

    # 9. Persistent disk identities, aligned with section 3's disk entries.
    if has_disk_ids:
        version, off = _get_u8(data, off)
        if version != 1:
            raise ValueError(f"unsupported disk identities version {version}")
        count, off = _get_u16(data, off)
        if count != len(devs):
            raise ValueError("disk identity count differs from disk count")
        for dev in devs:
            if len(data) - off < 2:
                raise ValueError("truncated disk id length")
            size = struct.unpack_from("<H", data, off)[0]
            if size > len(data) - off - 2:
                raise ValueError("truncated disk id")
            off += 2
            identity = data[off:off + size].decode("utf-8")
            off += size
            if identity:
                dev["id"] = identity

    # 10. TCP extension: rates the fixed block has no room for.
    if has_tcp_ext:
        if len(data) - off < 1:
            raise ValueError("truncated tcp extension version")
        version, off = _get_u8(data, off)
        if version != TCP_EXT_VERSION:
            raise ValueError(f"unsupported tcp extension version {version}")
        if len(data) - off < TCP_EXT_V1_SIZE:
            raise ValueError("truncated tcp extension")
        retrans, off = _get_f32(data, off)
        s.setdefault("tcp", {})["retrans_ps"] = retrans

    return s, off


# ---------------------------------------------------------------------------
# Top-level record decoder
# ---------------------------------------------------------------------------


def checked_record_body(payload: bytes) -> bytes:
    """Verify a checksummed payload and return the encoded sample inside it.

    The CRC-32 (IEEE, as zlib computes it) covers the 4-byte length prefix,
    then every payload byte before the trailer.
    """
    if len(payload) < 1 + RECORD_CHECKSUM_SIZE:
        raise ValueError(f"checksummed record too short: {len(payload)} bytes")
    end = len(payload) - RECORD_CHECKSUM_SIZE
    crc = zlib.crc32(struct.pack("<I", len(payload)))
    crc = zlib.crc32(payload[:end], crc)
    if crc != struct.unpack_from("<I", payload, end)[0]:
        raise ValueError("record checksum mismatch (torn by an unclean shutdown?)")
    return payload[1:end]


def decode_v2_record(payload: bytes) -> Optional[Dict[str, Any]]:
    """Decode a v2 binary AggregatedSample payload (no 4-byte length prefix).

    Raises ValueError for a checksummed record that fails its checksum.
    """
    if not payload:
        return None
    if payload[0] == RECORD_KIND_CHECKSUMMED:
        payload = checked_record_body(payload)
    elif payload[0] == RECORD_KIND_BINARY:
        payload = payload[1:]
    if len(payload) < 18:
        return None
    ts_ns = struct.unpack_from("<q", payload, 0)[0]
    dur_ns = struct.unpack_from("<q", payload, 8)[0]
    flags = struct.unpack_from("<H", payload, 16)[0]
    off = 18

    result: Dict[str, Any] = {
        # Presentation provenance only; no bytes are added to the record.
        "extrema_profile": (
            "none" if flags & 7 != 7 else
            "current" if flags & FLAG_REDUCER_V2 else
            "none" if flags & (FLAG_HAS_MEAN_STATS | FLAG_HAS_DISK_IDS) else
            "legacy"
        ),
        "timestamp": (
            datetime.datetime.fromtimestamp(ts_ns / 1e9).astimezone().isoformat()
            if ts_ns > 0
            else "(zero)"
        ),
        "duration_ms": round(dur_ns / 1e6, 3),
        "flags": {
            "has_data": bool(flags & FLAG_HAS_DATA),
            "has_min": bool(flags & FLAG_HAS_MIN),
            "has_max": bool(flags & FLAG_HAS_MAX),
            "has_apps": bool(flags & FLAG_HAS_APPS),
            "reducer_v2": bool(flags & FLAG_REDUCER_V2),
            "has_mean_stats": bool(flags & FLAG_HAS_MEAN_STATS),
            "has_apache2": bool(flags & FLAG_HAS_APACHE2),
            "has_mysql": bool(flags & FLAG_HAS_MYSQL),
            "has_psu": bool(flags & FLAG_HAS_PSU),
            "has_disk_ids": bool(flags & FLAG_HAS_DISK_IDS),
            "has_tcp_ext": bool(flags & FLAG_HAS_TCP_EXT),
        },
    }

    has_apps = bool(flags & FLAG_HAS_APPS)
    has_apache2 = bool(flags & FLAG_HAS_APACHE2)
    has_mysql = bool(flags & FLAG_HAS_MYSQL)
    has_psu = bool(flags & FLAG_HAS_PSU)
    has_disk_ids = bool(flags & FLAG_HAS_DISK_IDS)
    has_tcp_ext = bool(flags & FLAG_HAS_TCP_EXT)
    if has_disk_ids and (not has_apps or not has_psu):
        raise ValueError("disk identities require apps and PSU sections")
    for label, flag in [
        ("data", FLAG_HAS_DATA),
        ("min", FLAG_HAS_MIN),
        ("max", FLAG_HAS_MAX),
    ]:
        if flags & flag:
            block, off = _decode_fixed(payload, off)
            block, off = _decode_variable(
                payload, off, block, has_apps, has_apache2, has_mysql, has_psu,
                has_disk_ids, has_tcp_ext,
            )
            result[label] = block

    if flags & FLAG_HAS_MEAN_STATS:
        if len(payload) - off < 6:
            raise ValueError("truncated mean statistics header")
        version, complete, count = struct.unpack_from("<BBI", payload, off)
        off += 6
        if version != 1 or complete > 1 or count > (len(payload) - off) // 19:
            raise ValueError("invalid mean statistics header")
        stats = {}
        for _ in range(count):
            if len(payload) - off < 2:
                raise ValueError("truncated mean statistic path")
            size = struct.unpack_from("<H", payload, off)[0]
            off += 2
            if size == 0 or len(payload) - off < size + 16:
                raise ValueError("truncated mean statistic")
            path = payload[off:off + size].decode("utf-8")
            off += size
            total, weight = struct.unpack_from("<dd", payload, off)
            off += 16
            if path in stats or not math.isfinite(total) or not math.isfinite(weight) or weight < 0 or (weight == 0 and total != 0):
                raise ValueError("invalid mean statistic")
            stats[path] = {"sum": total, "weight": weight}
        if off != len(payload):
            raise ValueError("trailing mean statistics bytes")
        result["mean_weights_complete"] = bool(complete)
        result["mean_stats"] = stats
    return result


# ---------------------------------------------------------------------------
# Ring-buffer reader
# ---------------------------------------------------------------------------


class Ring:
    """Walks the records of a tier's data region the way the Go reader does.

    Offsets are data-region offsets (0 is the first byte after the header).
    In the active segment a bad length is a hole an unclean shutdown left,
    and the walk resyncs past it; in the old segment it is the end.
    """

    def __init__(self, buf: Any, max_data: int, codec_ver: int, newest_nano: int) -> None:
        self.buf = buf  # the whole file (mmap or bytes)
        self.max_data = max_data
        self.codec_ver = codec_ver or 1
        self.newest_nano = newest_nano

    def _u32(self, pos: int) -> int:
        return struct.unpack_from("<I", self.buf, HEADER_SIZE + pos)[0]

    def _byte(self, pos: int) -> int:
        return self.buf[HEADER_SIZE + pos]

    def valid_len(self, data_len: int) -> bool:
        """validRecordLen: no zero sentinel, within the ring and the cap."""
        return 0 < data_len <= MAX_RECORD_BYTES and data_len <= self.max_data

    def header_sane(self, pos: int, window_end: int) -> Optional[int]:
        """recordHeaderSane: the payload length of a plausible record at pos."""
        if window_end - pos < 13:
            return None
        data_len = self._u32(pos)
        if not self.valid_len(data_len):
            return None
        kind = self._byte(pos + 4)
        if kind in (RECORD_KIND_BINARY, RECORD_KIND_CHECKSUMMED):
            ts = struct.unpack_from("<q", self.buf, HEADER_SIZE + pos + 5)[0]
        elif self.codec_ver >= CODEC_V2:
            return None  # a v2 record always carries its kind byte
        else:
            ts = struct.unpack_from("<q", self.buf, HEADER_SIZE + pos + 4)[0]
        if ts < MIN_SANE_NS:
            return None
        if self.newest_nano > 0 and ts > self.newest_nano + MAX_AHEAD_NS:
            return None
        return data_len

    def intact(self, pos: int, record_len: int) -> bool:
        """peekedRecordIntact: a checksummed record verifies whole."""
        if record_len > SCAN_BUFFER:
            return True
        payload = self.buf[HEADER_SIZE + pos + 4:HEADER_SIZE + pos + record_len]
        try:
            checked_record_body(payload)
        except ValueError:
            return False
        return True

    def resync(self, pos: int, end: int) -> Optional[int]:
        """resyncToNextRecord: the next plausible record start in [pos, end)."""
        while end - pos >= 13:
            if self.buf[HEADER_SIZE + pos:HEADER_SIZE + pos + 4] == b"\0\0\0\0":
                # No record starts on a zero length: jump to 3 bytes before the
                # next non-zero byte, the first place one could.
                found = _NONZERO.search(self.buf, HEADER_SIZE + pos, HEADER_SIZE + end)
                if found is None:
                    return None
                pos = found.start() - HEADER_SIZE - 3
                continue
            window_end = min(pos + RESYNC_PEEK, end)
            data_len = self.header_sane(pos, window_end)
            if data_len is not None:
                record_len = 4 + data_len
                if pos + record_len > end:
                    pass  # runs past the segment: no record of it
                elif self._byte(pos + 4) == RECORD_KIND_CHECKSUMMED:
                    if self.intact(pos, record_len):
                        return pos
                elif pos + record_len == end:
                    return pos
                elif record_len + 13 <= window_end - pos:
                    if self.header_sane(pos + record_len, window_end) is not None:
                        return pos
                else:
                    return pos
            pos += 1
        return None

    def walk(self, start: int, size: int, resync: bool) -> Iterator[Tuple[int, int]]:
        """walkSegment: yields (offset, payload length) of each record."""
        pos, end = start, start + size
        while end - pos >= 4:
            data_len = self._u32(pos)
            if not self.valid_len(data_len) or pos + 4 + data_len > end:
                nxt = self.resync(pos, end) if resync else None
                if nxt is None:
                    return
                pos = nxt
                continue
            yield pos, data_len
            pos += 4 + data_len

    def payload(self, pos: int, data_len: int) -> bytes:
        return bytes(self.buf[HEADER_SIZE + pos + 4:HEADER_SIZE + pos + 4 + data_len])


def ring_segments(
    wrapped: bool, write_off: int, max_data: int, old_start: int, file_size: int
) -> List[Tuple[int, int, bool]]:
    """ringSegments: (start, size, resync) in chronological order.

    While wrapped, the previous pass [old_start, max_data) and then the active
    one [0, write_off); each is clipped to the bytes the file holds.
    """
    segments = []
    if wrapped:
        segments.append((old_start, max_data - old_start, False))
    segments.append((0, write_off, True))
    data_size = file_size - HEADER_SIZE
    return [(start, max(0, min(size, data_size - start)), resync) for start, size, resync in segments]


def tier_records(filepath: str) -> List[Tuple[int, int]]:
    """Every record (offset, payload length) a full scan visits, oldest first."""
    with open(filepath, "rb") as f:
        with mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ) as buf:
            ring, segments = open_ring(buf, parse_header(buf[:HEADER_SIZE]), len(buf))
            return [rec for start, size, resync in segments for rec in ring.walk(start, size, resync)]


def timestamp_at(buf: Any, max_data: int, file_size: int, pos: int) -> Optional[int]:
    """readTimestampAt: the timestamp (ns) of the record at pos, if readable."""
    if HEADER_SIZE + pos + 13 > file_size:
        return None
    data_len, kind = struct.unpack_from("<IB", buf, HEADER_SIZE + pos)
    if not 0 < data_len <= min(MAX_RECORD_BYTES, max_data):
        return None
    if kind in (RECORD_KIND_BINARY, RECORD_KIND_CHECKSUMMED):
        return struct.unpack_from("<q", buf, HEADER_SIZE + pos + 5)[0] if data_len >= 9 else None
    if kind == ord("{"):
        return None  # JSON tiers predate tail tracking, so no tail to validate
    return struct.unpack_from("<q", buf, HEADER_SIZE + pos + 4)[0] if data_len >= 8 else None


def ring_layout(buf: Any, header: Tuple, file_size: int) -> Tuple[bool, int]:
    """readHeader's wrap state: (wrapped, offset of the oldest record).

    A persisted tail is trusted only when it decodes to a sane oldest record.
    Otherwise (and for files written before tail tracking) a file larger than
    [0, write_off) has wrapped, and its old pass is taken to start at
    write_off, as the reader before tail tracking did.
    """
    _, header_flags, _, max_data, write_off, count, _, newest_nano, oldest_off = header
    if header_flags & FLAG_HAS_TAIL:
        if not header_flags & FLAG_WRAPPED:
            return False, 0
        if write_off <= oldest_off < max_data:
            ts = timestamp_at(buf, max_data, file_size, oldest_off)
            if ts is not None and (newest_nano <= 0 or ts <= newest_nano):
                return True, oldest_off
    if count > 0 and file_size > HEADER_SIZE + write_off:
        return True, write_off
    return False, 0


def open_ring(buf: Any, header: Tuple, file_size: int) -> Tuple[Ring, List[Tuple[int, int, bool]]]:
    """The walker and segments for a parsed header."""
    _, _, codec_ver, max_data, write_off, _, _, newest_nano, _ = header
    wrapped, old_start = ring_layout(buf, header, file_size)
    ring = Ring(buf, max_data, codec_ver, newest_nano)
    return ring, ring_segments(wrapped, write_off, max_data, old_start, file_size)


# ---------------------------------------------------------------------------
# Pretty printer
# ---------------------------------------------------------------------------


def print_record(payload: bytes, codec_ver: int) -> None:
    """Print a single record according to its codec version."""
    if codec_ver >= CODEC_V2:
        try:
            parsed = decode_v2_record(payload)
            print("\nLatest Record (v2 binary):")
            print(json.dumps(parsed, indent=2))
        except Exception as exc:  # pylint: disable=broad-exception-caught
            print(
                f"\nLatest Record (v2 binary, decode error: {exc}): {payload[:32]!r}…"
            )
    else:
        try:
            parsed = json.loads(payload.decode("utf-8"))
            print("\nLatest Record (v1 JSON):")
            print(json.dumps(parsed, indent=2))
        except (json.JSONDecodeError, UnicodeDecodeError):
            print(f"\nLatest Record (v1 JSON, decode failed): {payload!r}")


# ---------------------------------------------------------------------------
# Main inspector
# ---------------------------------------------------------------------------


def inspect_tier(filepath: str) -> None:
    """Inspect a tier file and print its header and latest record."""
    try:
        with open(filepath, "rb") as f:
            file_size = os.fstat(f.fileno()).st_size
            if file_size < HEADER_SIZE:
                print(
                    f"Error: File too small ({file_size} bytes, expected {HEADER_SIZE})",
                    file=sys.stderr,
                )
                sys.exit(1)
            with mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ) as buf:
                print_tier(filepath, buf, file_size)
    except OSError as err:
        print(f"Error inspecting tier file: {err}", file=sys.stderr)
        sys.exit(1)


def print_tier(filepath: str, buf: Any, file_size: int) -> None:
    """Print the header of a mapped tier file and its latest record."""
    header = parse_header(buf[:HEADER_SIZE])
    magic, _, codec_ver, max_data, write_off, count, oldest_nano, newest_nano, _ = header
    if magic != MAGIC:
        print(
            f"Error: Invalid magic: {magic.decode('utf-8', errors='replace')}",
            file=sys.stderr,
        )
        sys.exit(1)

    wrapped, _ = ring_layout(buf, header, file_size)
    codec_label = "v2 binary" if codec_ver >= CODEC_V2 else "v1 JSON (legacy)"

    print(f"File:          {filepath}")
    print(f"Codec:         {codec_label} (version={codec_ver})")
    current_data = max_data if wrapped else write_off
    pct = (current_data / max_data * 100) if max_data > 0 else 0.0
    print(f"Data Size:     {current_data:,} / {max_data:,} bytes ({pct:.2f}%)")
    print(f"Write Offset:  {write_off:,}")
    print(f"Total Records: {count:,}")
    oldest_ts = (
        datetime.datetime.fromtimestamp(oldest_nano / 1e9).astimezone()
        if oldest_nano > 0
        else None
    )
    newest_ts = (
        datetime.datetime.fromtimestamp(newest_nano / 1e9).astimezone()
        if newest_nano > 0
        else None
    )
    print(f"Oldest:        {oldest_ts.isoformat() if oldest_ts else '(none)'}")
    print(f"Newest:        {newest_ts.isoformat() if newest_ts else '(none)'}")
    print(f"Wrapped:       {wrapped}")
    if oldest_ts and newest_ts:
        print(f"Time Covered:  {newest_ts - oldest_ts}")

    if count == 0:
        print("\nLatest Record: (none)")
        return

    ring, segments = open_ring(buf, header, file_size)
    latest = None
    for start, size, resync in segments:
        for record in ring.walk(start, size, resync):
            latest = record
    if latest:
        print_record(ring.payload(*latest), codec_ver)
    else:
        print("\nLatest Record: (none found)")


def main(argv: Optional[List[str]] = None) -> None:
    import argparse

    parser = argparse.ArgumentParser(
        description="Show a Kula tier file's header, coverage and latest record.",
        epilog="Use -- before a path that starts with a dash.",
    )
    parser.add_argument("path", help="tier file, e.g. /var/lib/kula/tier_0.dat")
    inspect_tier(parser.parse_args(argv).path)


if __name__ == "__main__":
    main()
