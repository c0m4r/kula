"""HTTP traffic for the showcase's web servers, so their charts move.

Usage: web_traffic.py HOST [HOST ...]

For each host, a few clients send requests at a rate that rises and falls over
ten minutes, sometimes on a fresh connection, and a few idle keep-alive
connections stay open, so nginx shows waiting connections and Apache busy and
idle workers.
"""

import http.client
import math
import random
import sys
import threading
import time

CLIENTS = 4  # per host
IDLE = 3  # idle keep-alive connections per host
PATHS = ["/", "/", "/", "/missing"]


def rate(elapsed: float) -> float:
    """Requests per second for one host: 4 to 30 over a ten-minute cycle."""
    return 4 + 26 * (1 + math.sin(2 * math.pi * elapsed / 600)) / 2


def client(host: str, seed: int) -> None:
    rnd = random.Random(seed)
    start = time.monotonic()
    conn = None
    while True:
        try:
            if conn is None:
                conn = http.client.HTTPConnection(host, 80, timeout=10)
            conn.request("GET", rnd.choice(PATHS))
            conn.getresponse().read()
            if rnd.random() < 0.2:  # a new connection now and then: accepts/s
                conn.close()
                conn = None
        except (OSError, http.client.HTTPException):
            if conn is not None:
                conn.close()
            conn = None
            time.sleep(2)
            continue
        time.sleep(rnd.expovariate(rate(time.monotonic() - start) / CLIENTS))


def idler(host: str) -> None:
    while True:
        try:
            conn = http.client.HTTPConnection(host, 80, timeout=10)
            while True:
                conn.request("GET", "/")
                conn.getresponse().read()
                time.sleep(4)  # under Apache's 5-second KeepAliveTimeout
        except (OSError, http.client.HTTPException):
            time.sleep(2)


def main() -> None:
    hosts = sys.argv[1:]
    if not hosts:
        sys.exit("usage: web_traffic.py HOST [HOST ...]")
    for h, host in enumerate(hosts):
        for i in range(CLIENTS):
            threading.Thread(
                target=client, args=(host, h * 100 + i), daemon=True
            ).start()
        for _ in range(IDLE):
            threading.Thread(target=idler, args=(host,), daemon=True).start()
    print(f"sending traffic to {', '.join(hosts)}")
    threading.Event().wait()


if __name__ == "__main__":
    main()
