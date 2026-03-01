# Kula-Szpiegula – Walkthrough

## What Was Built

A **standalone, self-contained Linux server monitoring tool** in Go. Single 11MB binary, no external databases.

### Architecture
```
Collectors → Ring-Buffer Storage → Web API / WebSocket → Dashboard
     ↓
    TUI
```

### Files Created

| Component | Files | Purpose |
|-----------|-------|---------|
| **Config** | config.go, config.example.yaml | YAML config with defaults |
| **Collectors** | types.go, collector.go, cpu.go, network.go, disk.go, system.go, process.go, self.go | Read `/proc` and `/sys` every 1s |
| **Storage** | store.go, tier.go, codec.go | 3-tier ring-buffer (1s/1m/5m) |
| **TUI** | tui.go | bubbletea dashboard |
| **Web** | server.go, websocket.go, auth.go | HTTP + WS + Argon2id auth |
| **Frontend** | index.html, style.css, app.js | Chart.js dashboard SPA |
| **Entry** | main.go | CLI with serve/tui/hash-password |

### Metrics Collected
- **CPU**: per-core + total (user, nice, system, idle, iowait, irq, softirq, steal, guest)
- **Load Average**: 1/5/15 min, running/total tasks
- **Memory**: total, free, available, used, buffers, cached, sreclaimable, shmem, dirty, mapped
- **Swap**: total, free, used, cached
- **Network**: per-interface rx/tx bytes/Mbps/packets/errors/drops, TCP/UDP/ICMP counters (IPv4+IPv6), socket stats
- **Disk**: per-device reads/writes/utilization, filesystem usage with inodes
- **System**: uptime, entropy, clock sync, hostname, logged-in users
- **Processes**: total, running, sleeping, blocked, zombie, threads
- **Self**: own CPU%, RSS, VMS, threads, file descriptors

## Verification

### Build
```
✓ go build → 11MB ELF 64-bit binary, zero errors
```

### Runtime Test
```
✓ Server starts, collects metrics every 1s
✓ GET /api/current returns full JSON with all metrics
✓ GET / serves HTML dashboard
✓ SIGINT cleanly shuts down
```

### Key Commands
```bash
# Build
go build -o kula ./cmd/kula/

# Run server mode
./kula --config=config.yaml serve

# Run TUI mode  
./kula --config=config.yaml tui

# Generate password hash
./kula hash-password
```
