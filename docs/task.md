# Kula-Szpiegula: Linux Server Monitoring Tool

## Phase 1: Foundation
- [x] Create project structure and Go module
- [x] Implement configuration system (YAML config)
- [x] Implement metric collectors (CPU, RAM, SWAP, Load, Network, Disk, Processes, etc.)
- [x] Implement tiered binary storage engine

## Phase 2: TUI
- [x] Implement terminal UI with live metrics dashboard

## Phase 3: Web UI Backend
- [x] Implement HTTP server with REST API
- [x] Implement WebSocket for live streaming
- [x] Implement Argon2id authentication system

## Phase 4: Web UI Frontend
- [x] Build responsive dashboard with gauge charts
- [x] Build detailed history graphs with time window selection
- [x] Implement live updates, pause/resume, time range presets

## Phase 5: Polish & Verification
- [x] Build compiles successfully (11MB binary)
- [x] API returns valid JSON with all metrics
- [x] HTML/CSS/JS frontend served correctly
- [x] Server starts/stops cleanly
- [ ] User visual verification of Web UI (no Chrome available)
