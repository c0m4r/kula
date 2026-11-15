"""Feeds the showcase's custom chart groups through Kula's Unix socket.

Usage: custom_metrics.py SOCKET

Sends one newline-delimited JSON message a second, on one connection kept
open, for the groups config.template.yaml declares under applications.custom:
room (temperature, humidity) and queue (depth, consumers).
"""

import json
import math
import random
import socket
import sys
import time
from typing import Any


def sample(t: float, depth: float, consumers: int) -> dict[str, Any]:
    return {
        "custom": {
            "room": [
                {
                    "temperature": round(
                        21 + 3 * math.sin(t / 300) + random.gauss(0, 0.15), 2
                    )
                },
                {
                    "humidity": round(
                        45 + 10 * math.sin(t / 450 + 1) + random.gauss(0, 0.5), 1
                    )
                },
            ],
            "queue": [
                {"depth": round(depth)},
                {"consumers": consumers},
            ],
        }
    }


def main() -> None:
    if len(sys.argv) != 2:
        sys.exit("usage: custom_metrics.py SOCKET")
    path = sys.argv[1]
    depth, consumers = 20.0, 2
    conn = None
    start = time.monotonic()
    while True:
        # Jobs arrive in bursts; consumers scale up when the queue grows and
        # back down when it drains.
        depth += random.expovariate(1 / 6) * (3 if random.random() < 0.05 else 1)
        depth = max(0.0, depth - consumers * random.uniform(0.8, 1.6))
        if depth > 120 and consumers < 10:
            consumers += 1
        elif depth < 20 and consumers > 1:
            consumers -= 1
        line = json.dumps(sample(time.monotonic() - start, depth, consumers)) + "\n"
        try:
            if conn is None:
                conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                conn.connect(path)
                print(f"connected to {path}")
            conn.sendall(line.encode())
        except OSError as err:
            if conn is not None:
                conn.close()
            conn = None
            print(f"{path}: {err}; retrying")
            time.sleep(4)
        time.sleep(1)


if __name__ == "__main__":
    main()
