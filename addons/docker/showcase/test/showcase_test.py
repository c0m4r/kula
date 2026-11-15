"""Checks every Kula feature in the running showcase stack.

Run by ./showcase.sh test, in the tester service: host network, Kula's uid and
gid, the data volume read-only at /kula and the generated secrets at /secrets.
Prints PASS, FAIL or SKIP per check and exits 1 when any check fails.
"""

import base64
import http.client
import json
import os
import socket
import ssl
import sys
import time
import urllib.parse
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable, TypeVar

DIRECT = "http://127.0.0.1:27970/kula"
PROXY = "https://localhost:27971/kula"
PROMETHEUS = "http://127.0.0.1:27972"
ORIGIN_DIRECT = "http://127.0.0.1:27970"
ORIGIN_PROXY = "https://localhost:27971"

ADMIN_PASSWORD = os.environ["KULA_ADMIN_PASSWORD"]
VIEWER_PASSWORD = os.environ["KULA_VIEWER_PASSWORD"]
TOKEN = os.environ["PROMETHEUS_TOKEN"]
AI = os.environ.get("SHOWCASE_AI") == "1"
MODEL = os.environ.get("OLLAMA_MODEL", "")
LOGINS = os.environ.get("SHOWCASE_LOGINS") == "1"

# Caddy issues the proxy's certificate from its own local CA; showcase.sh
# copies that CA's root certificate out of the proxy container.
def tls_context() -> ssl.SSLContext:
    ctx = ssl.create_default_context(cafile=os.environ["CADDY_ROOT_CA"])
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    return ctx


TLS = tls_context()

RATE_LIMITED = (
    "Kula's login limit: 5 attempts per account and address in 5 minutes, "
    "successful ones too; wait and run the test again"
)

T = TypeVar("T")
Check = Callable[[], "str | None"]


class Skip(Exception):
    """A check that does not apply to this stack."""


class Failures:
    count = 0


class Response:
    """An HTTP response, read in full."""

    def __init__(self, status: int, headers: http.client.HTTPMessage, body: bytes):
        self.status, self.headers, self.body = status, headers, body

    def json(self) -> Any:
        return json.loads(self.body)

    def text(self) -> str:
        return self.body.decode(errors="replace")


def request(
    url: str,
    method: str = "GET",
    body: Any = None,
    headers: dict[str, str] | None = None,
    timeout: float = 15,
) -> Response:
    u = urllib.parse.urlsplit(url)
    host, port = u.hostname or "", u.port
    conn: http.client.HTTPConnection
    if u.scheme == "https":
        conn = http.client.HTTPSConnection(host, port, timeout=timeout, context=TLS)
    else:
        conn = http.client.HTTPConnection(host, port, timeout=timeout)
    path = u.path + ("?" + u.query if u.query else "")
    data = json.dumps(body).encode() if body is not None else None
    hdrs = dict(headers or {})
    if data is not None:
        hdrs["Content-Type"] = "application/json"
    try:
        conn.request(method, path, body=data, headers=hdrs)
        resp = conn.getresponse()
        return Response(resp.status, resp.headers, resp.read())
    finally:
        conn.close()


def expect(cond: object, message: str) -> None:
    if not cond:
        raise AssertionError(message)


def eventually(fn: Callable[[], T], timeout: float, interval: float = 2) -> T:
    """Retries fn until it stops failing an expectation, or timeout passes."""
    deadline = time.monotonic() + timeout
    while True:
        try:
            return fn()
        except (AssertionError, OSError) as err:
            if time.monotonic() >= deadline:
                raise AssertionError(f"{err} (after {timeout:g}s)") from None
            time.sleep(interval)


def check(name: str, fn: Check) -> None:
    try:
        detail = fn()
        print(f"PASS {name}" + (f" — {detail}" if detail else ""), flush=True)
    except Skip as err:
        print(f"SKIP {name} — {err}", flush=True)
    except (AssertionError, OSError, ValueError, KeyError) as err:
        Failures.count += 1
        print(f"FAIL {name} — {err}", flush=True)


class Session:
    """A login. Each test run logs in once per account, under the limit."""

    def __init__(self, base: str, origin: str, username: str, password: str):
        self.base = base
        r = request(
            f"{base}/api/login",
            "POST",
            {"username": username, "password": password},
            {"Origin": origin},
        )
        expect(r.status != 429, RATE_LIMITED)
        expect(r.status == 200, f"login as {username}: HTTP {r.status} {r.text()[:80]}")
        self.set_cookie = r.headers.get("Set-Cookie", "")
        cookie = self.set_cookie.split(";", 1)[0]
        expect(cookie.startswith("kula_session="), "no kula_session cookie")
        self.headers = {
            "Cookie": cookie,
            "Origin": origin,
            "X-CSRF-Token": r.json()["csrf_token"],
        }

    def get(self, path: str) -> Response:
        return request(self.base + path, headers=self.headers)

    def post(self, path: str, body: Any = None, timeout: float = 15) -> Response:
        return request(self.base + path, "POST", body, self.headers, timeout)


def websocket_frame(base: str, session: Session) -> Any:
    """Opens /ws as the dashboard does and returns its first text frame."""
    u = urllib.parse.urlsplit(base + "/ws")
    host = u.hostname or ""
    raw = socket.create_connection((host, u.port or 80), timeout=10)
    sock = TLS.wrap_socket(raw, server_hostname=host) if u.scheme == "https" else raw
    key = base64.b64encode(os.urandom(16)).decode()
    lines = [
        f"GET {u.path} HTTP/1.1",
        f"Host: {u.netloc}",
        "Upgrade: websocket",
        "Connection: Upgrade",
        f"Sec-WebSocket-Key: {key}",
        "Sec-WebSocket-Version: 13",
        f"Origin: {session.headers['Origin']}",
        f"Cookie: {session.headers['Cookie']}",
    ]
    buf = b""

    def read(n: int) -> bytes:
        nonlocal buf
        while len(buf) < n:
            chunk = sock.recv(65536)
            expect(chunk, "connection closed early")
            buf += chunk
        out, buf = buf[:n], buf[n:]
        return out

    with sock:
        sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
        while b"\r\n\r\n" not in buf:
            chunk = sock.recv(4096)
            expect(chunk, "connection closed during the handshake")
            buf += chunk
        head, buf = buf.split(b"\r\n\r\n", 1)
        status = head.split(b"\r\n", 1)[0].decode()
        expect(" 101 " in status + " ", f"handshake answered {status}")
        while True:
            b0, b1 = read(2)
            length = b1 & 0x7F
            if length == 126:
                length = int.from_bytes(read(2), "big")
            elif length == 127:
                length = int.from_bytes(read(8), "big")
            payload = read(length)
            if b0 & 0x0F == 1:  # a text frame
                return json.loads(payload)


def serving_and_auth() -> Session:
    check(
        "health under web.base_path",
        lambda: expect(
            request(f"{DIRECT}/health").text() == "kula is healthy", "not healthy"
        ),
    )
    check(
        "nothing served outside web.base_path",
        lambda: expect(
            request("http://127.0.0.1:27970/health").status == 404, "answered"
        ),
    )
    check(
        "API refuses requests without a session",
        lambda: expect(request(f"{DIRECT}/api/current").status == 401, "not 401"),
    )

    def wrong_login() -> None:
        # A name of its own, so the refusal costs admin and viewer no attempt
        r = request(
            f"{DIRECT}/api/login",
            "POST",
            {"username": "showcase-test", "password": "wrong"},
            {"Origin": ORIGIN_DIRECT},
        )
        expect(r.status != 429, RATE_LIMITED)
        expect(r.status == 401, f"HTTP {r.status}")

    check("login refuses wrong credentials", wrong_login)

    admin = Session(DIRECT, ORIGIN_DIRECT, "admin", ADMIN_PASSWORD)
    check("login as admin", lambda: None)
    check(
        "read-only routes refuse POST",
        lambda: expect(admin.post("/api/current").status == 405, "not 405"),
    )
    return admin


def app_check(
    admin: Session,
    key: str,
    test: Callable[[Any], None],
    describe: Callable[[Any], str],
) -> Check:
    """Waits for apps.<key> in /api/current to pass test, then describes it."""

    def once() -> str:
        found = (admin.get("/api/current").json().get("apps") or {}).get(key)
        expect(found, f"no apps.{key} yet")
        test(found)
        return describe(found)

    return lambda: eventually(once, 120)


def containers_named(containers: Any) -> None:
    names = [c["name"] for c in containers]
    expect(
        any(n.startswith("kula-showcase-") for n in names),
        f"no showcase container by name (socket mode?): {names[:5]}",
    )


def describe_containers(containers: Any) -> str:
    ours = sum(c["name"].startswith("kula-showcase-") for c in containers)
    return f"{len(containers)} containers, {ours} of them the showcase's"


def describe_custom(groups: Any) -> str:
    return ", ".join(
        f"{g}: " + " ".join(f"{m['name']}={m['value']:g}" for m in groups[g])
        for g in ("room", "queue")
    )


def dashboard_api(admin: Session) -> None:
    def system() -> str:
        cur = admin.get("/api/current").json()
        expect(cur["cpu"]["num_cores"] > 0, "no CPU cores")
        expect(cur["mem"]["total"] > 0, "no memory total")
        expect(cur["net"], "no network stats")
        return f"{cur['cpu']['num_cores']} cores"

    check("system metrics in /api/current", system)
    check(
        "nginx (stub_status)",
        app_check(
            admin,
            "nginx",
            lambda n: expect(n["requests_ps"] > 0, "no requests/s yet"),
            lambda n: f"{n['requests_ps']:.1f} req/s, {n['active_conn']} active",
        ),
    )
    check(
        "Apache2 (mod_status)",
        app_check(
            admin,
            "apache2",
            lambda a: expect(a["accesses_ps"] > 0, "no accesses/s yet"),
            lambda a: f"{a['accesses_ps']:.1f} req/s, {a['busy_workers']} busy workers",
        ),
    )
    check(
        "PostgreSQL",
        app_check(
            admin,
            "postgres",
            lambda p: expect(p["max_conns"] > 0, "no max connections"),
            lambda p: f"{p['active_conns']} active, {p['idle_in_tx_conns']} idle in transaction",
        ),
    )
    check(
        "MariaDB",
        app_check(
            admin,
            "mysql",
            lambda m: expect(m["queries_ps"] > 0, "no queries/s yet"),
            lambda m: f"{m['queries_ps']:.0f} queries/s, {m['threads_connected']} threads",
        ),
    )
    check(
        "containers by name (Docker socket)",
        app_check(admin, "containers", containers_named, describe_containers),
    )
    check(
        "custom metrics through kula.sock",
        app_check(
            admin,
            "custom",
            lambda c: expect({"room", "queue"} <= set(c), f"groups {sorted(c)}"),
            describe_custom,
        ),
    )

    def history() -> str:
        to = datetime.now(timezone.utc)
        stamp = "%Y-%m-%dT%H:%M:%SZ"
        q = urllib.parse.urlencode(
            {
                "from": (to - timedelta(minutes=5)).strftime(stamp),
                "to": to.strftime(stamp),
            }
        )
        r = admin.get(f"/api/history?{q}")
        expect(r.status == 200, f"HTTP {r.status}")
        return f"{len(r.body)} bytes for the last 5 minutes"

    check("history API", history)
    check(
        "config API lists the custom chart groups",
        lambda: expect(
            '"room"' in admin.get("/api/config").text(), "custom groups missing"
        ),
    )
    check(
        "system info API",
        lambda: expect(admin.get("/api/system-info").status == 200, "not 200"),
    )
    check(
        "translations API",
        lambda: expect(admin.get("/api/i18n?lang=de").status == 200, "not 200"),
    )
    check(
        "live WebSocket",
        lambda: f"first frame has {len(websocket_frame(DIRECT, admin))} keys",
    )


def reverse_proxy() -> None:
    check(
        "health through the Caddy proxy (HTTPS)",
        lambda: expect(
            request(f"{PROXY}/health").text() == "kula is healthy", "not healthy"
        ),
    )

    def viewer_through_proxy() -> str:
        s = Session(PROXY, ORIGIN_PROXY, "viewer", VIEWER_PASSWORD)
        expect("Secure" in s.set_cookie, f"cookie not Secure: {s.set_cookie}")
        r = s.get("/api/current")
        expect(r.status == 200, f"HTTP {r.status}")
        expect(r.headers.get("Strict-Transport-Security"), "no HSTS header")
        websocket_frame(PROXY, s)
        return "Secure cookie, HSTS and WebSocket through X-Forwarded-Proto"

    check(
        "second account (viewer): login, API and WebSocket through the proxy",
        viewer_through_proxy,
    )


def metrics_text() -> str:
    r = request(f"{DIRECT}/metrics", headers={"Authorization": f"Bearer {TOKEN}"})
    expect(r.status == 200, f"HTTP {r.status}")
    return r.text()


def prometheus() -> None:
    check(
        "/metrics refuses a scrape without the token",
        lambda: expect(request(f"{DIRECT}/metrics").status == 401, "not 401"),
    )

    def every_application() -> str:
        text = metrics_text()
        names = [
            "kula_cpu_usage_percent",
            "kula_nginx_requests_total",
            "kula_apache2_",
            "kula_postgres_",
            "kula_mysql_",
            "kula_container_cpu_percent",
            "kula_custom_metric",
        ]
        missing = [n for n in names if n not in text]
        expect(not missing, f"missing {missing}")
        samples = sum(
            1 for line in text.splitlines() if line and not line.startswith("#")
        )
        return f"{samples} samples"

    check("/metrics with the bearer token", lambda: eventually(every_application, 60))

    def query(q: str) -> Any:
        r = request(
            f"{PROMETHEUS}/api/v1/query?" + urllib.parse.urlencode({"query": q})
        )
        expect(r.status == 200, f"Prometheus HTTP {r.status}")
        return r.json()["data"]["result"]

    def scraped() -> str:
        up = query('up{job="kula"}')
        expect(up and up[0]["value"][1] == "1", f'up{{job="kula"}} is {up}')
        series = query("kula_custom_metric")
        expect(series, "no kula_custom_metric series yet")
        return f"up=1, {len(series)} custom metric series"

    check("Prometheus scrapes Kula", lambda: eventually(scraped, 60))

    def logins() -> None:
        if not LOGINS:
            raise Skip("the host has neither /run/utmp nor /run/systemd/sessions")
        expect(
            "kula_system_logged_in_users" in metrics_text(), "no logged-in user count"
        )

    check("logged-in users from the host's login records", logins)


def backups() -> None:
    def latest() -> str:
        root = Path("/kula/backup")
        runs = sorted(p for p in root.glob("*") if (p / "tier_0.dat.gz").exists())
        expect(runs, "no backup yet (cron */2)")
        tiers = " ".join(sorted(f.name for f in runs[-1].iterdir()))
        return f"{len(runs)} runs, latest {runs[-1].name}: {tiers}"

    check("scheduled backups", lambda: eventually(latest, 150, 5))


def ai(admin: Session) -> None:
    if not AI:

        def off() -> None:
            raise Skip("start with ./showcase.sh up --ai")

        check("AI assistant (Ollama)", off)
        return

    def model_listed() -> str:
        r = admin.get("/api/ollama/models")
        expect(r.status == 200, f"HTTP {r.status} {r.text()[:80]}")
        expect(
            MODEL in r.text(),
            f"{MODEL} not pulled yet (docker compose logs ollama-pull)",
        )
        return MODEL

    check("AI model list", lambda: eventually(model_listed, 900, 10))

    def answer() -> str:
        r = admin.post(
            "/api/ollama/chat",
            {
                "prompt": "In one short sentence: how busy is this server?",
                "context": "current",
                "lang": "en",
            },
            timeout=300,
        )
        expect(r.status == 200, f"HTTP {r.status} {r.text()[:80]}")
        text, event = "", ""
        for line in r.text().splitlines():
            if line.startswith("event: "):
                event = line[7:]
            elif line.startswith("data: "):
                expect(event != "error", f"stream error: {line[6:]}")
                if not event:
                    text += json.loads(line[6:])
            elif not line:
                event = ""
        expect(text.strip(), "empty answer")
        return text.strip().replace("\n", " ")[:120]

    check("AI chat answers about the current metrics", answer)


def main() -> int:
    try:
        admin = serving_and_auth()
        dashboard_api(admin)
        reverse_proxy()
        prometheus()
        backups()
        ai(admin)
    except (AssertionError, OSError) as err:
        Failures.count += 1
        print(f"FAIL {err}", flush=True)
    return 1 if Failures.count else 0


if __name__ == "__main__":
    sys.exit(main())
