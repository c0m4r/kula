#!/usr/bin/env python3
"""
Minimal HTTP reverse proxy for testing Kula's Unix socket listener.

Listens on a TCP port and forwards every request (including WebSocket upgrades)
to a Kula instance bound to a Unix domain socket. Useful for quickly verifying
that `web.unix_socket: /run/kula/kula.sock` works end-to-end without nginx.

Usage:
    ./reverse_proxy.py [--listen 127.0.0.1:8080] [--socket /run/kula/kula.sock]

Then open http://127.0.0.1:8080/ in a browser.

Stdlib only — no dependencies.
"""

import argparse
import http.client
import select
import socket
import socketserver
import sys
from http.server import BaseHTTPRequestHandler

HOP_BY_HOP = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
}


class UnixHTTPConnection(http.client.HTTPConnection):
    """HTTPConnection that dials a Unix domain socket instead of TCP."""

    def __init__(self, socket_path: str, timeout: float = 30.0) -> None:
        super().__init__("localhost", timeout=timeout)
        self._socket_path = socket_path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self._socket_path)
        self.sock = sock


def _is_websocket(headers) -> bool:
    return (
        headers.get("Upgrade", "").lower() == "websocket"
        and "upgrade" in headers.get("Connection", "").lower()
    )


def _filter_headers(headers, drop_extra=None):
    drop = HOP_BY_HOP | {h.lower() for h in (drop_extra or [])}
    return [(k, v) for k, v in headers.items() if k.lower() not in drop]


def make_handler(socket_path: str):
    """Build a request handler class bound to the configured Unix socket."""

    class ProxyHandler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt, *args):  # noqa: N802
            sys.stderr.write(
                f"[{self.log_date_time_string()}] {self.address_string()} {fmt % args}\n"
            )

        def _proxy(self):
            if _is_websocket(self.headers):
                self._proxy_websocket()
                return
            self._proxy_http()

        def _proxy_http(self):
            body = None
            length = self.headers.get("Content-Length")
            if length is not None:
                try:
                    body = self.rfile.read(int(length))
                except ValueError:
                    self.send_error(400, "Invalid Content-Length")
                    return

            conn = UnixHTTPConnection(socket_path)
            try:
                headers = dict(_filter_headers(self.headers))
                headers.setdefault("Host", "kula.local")
                headers["X-Forwarded-For"] = self.client_address[0]
                headers["X-Forwarded-Proto"] = "http"

                conn.request(self.command, self.path, body=body, headers=headers)
                resp = conn.getresponse()

                self.send_response(resp.status, resp.reason)
                for k, v in _filter_headers(resp.headers):
                    self.send_header(k, v)
                payload = resp.read()
                if "content-length" not in {
                    h.lower() for h, _ in _filter_headers(resp.headers)
                }:
                    self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                if payload and self.command != "HEAD":
                    self.wfile.write(payload)
            except (ConnectionError, FileNotFoundError, socket.error) as exc:
                self.send_error(502, f"Upstream socket error: {exc}")
            finally:
                conn.close()

        def _proxy_websocket(self):
            """Tunnel a WebSocket upgrade by relaying raw bytes after the handshake."""
            try:
                upstream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                upstream.connect(socket_path)
            except (ConnectionError, FileNotFoundError, socket.error) as exc:
                self.send_error(502, f"Upstream socket error: {exc}")
                return

            try:
                req_lines = [f"{self.command} {self.path} HTTP/1.1\r\n"]
                hdrs = dict(
                    _filter_headers(self.headers, drop_extra=["Connection", "Upgrade"])
                )
                hdrs.setdefault("Host", "kula.local")
                hdrs["Connection"] = "Upgrade"
                hdrs["Upgrade"] = self.headers.get("Upgrade", "websocket")
                hdrs["X-Forwarded-For"] = self.client_address[0]
                hdrs["X-Forwarded-Proto"] = "http"
                for k, v in hdrs.items():
                    req_lines.append(f"{k}: {v}\r\n")
                req_lines.append("\r\n")
                upstream.sendall("".join(req_lines).encode("latin-1"))

                client_sock = self.connection
                self._splice(client_sock, upstream)
            finally:
                upstream.close()

        @staticmethod
        def _splice(a: socket.socket, b: socket.socket) -> None:
            a.setblocking(False)
            b.setblocking(False)
            socks = [a, b]
            while True:
                readable, _, errored = select.select(socks, [], socks, 60)
                if errored:
                    return
                if not readable:
                    return
                for src in readable:
                    dst = b if src is a else a
                    try:
                        data = src.recv(65536)
                    except (BlockingIOError, InterruptedError):
                        continue
                    if not data:
                        return
                    try:
                        dst.sendall(data)
                    except (ConnectionError, OSError):
                        return

        # Map every HTTP verb to the same proxy implementation.
        do_GET = _proxy
        do_POST = _proxy
        do_PUT = _proxy
        do_DELETE = _proxy
        do_PATCH = _proxy
        do_HEAD = _proxy
        do_OPTIONS = _proxy

    return ProxyHandler


class ThreadedHTTPServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    daemon_threads = True
    allow_reuse_address = True


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--listen",
        default="127.0.0.1:8080",
        help="host:port to bind (default: 127.0.0.1:8080)",
    )
    parser.add_argument(
        "--socket",
        default="/run/kula/kula.sock",
        help="path to Kula's Unix socket (default: /run/kula/kula.sock)",
    )
    args = parser.parse_args()

    host, _, port = args.listen.rpartition(":")
    if not host or not port.isdigit():
        parser.error(f"invalid --listen value: {args.listen!r}")

    handler = make_handler(args.socket)
    server = ThreadedHTTPServer((host, int(port)), handler)
    print(f"Reverse proxy listening on http://{host}:{port} -> unix:{args.socket}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("Shutting down.")
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
