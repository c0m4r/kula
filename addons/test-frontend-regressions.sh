#!/usr/bin/env bash

set -e

GREEN="\033[0;32m"
CYAN="\033[0;36m"
RED="\033[0;31m"
RESET="\033[0m"

cd "$(dirname "$0")/.."

if command -v node &>/dev/null && { [ -n "${KULA_CHROMIUM:-}" ] || command -v chromium &>/dev/null || command -v chromium-browser &>/dev/null || command -v google-chrome &>/dev/null || command -v google-chrome-stable &>/dev/null; }; then
    echo -e "${CYAN}Running frontend browser regressions...${RESET}"
    node --experimental-websocket internal/web/testdata/history_dashboard_test.mjs
    node --experimental-websocket internal/web/testdata/history_performance_test.mjs
else
    echo -e "${CYAN}Skipping frontend browser regressions (Node.js or Chromium/Chrome not installed)${RESET}"
fi

echo -e "\n🎉 All checks ${GREEN}passed!${RESET}"
