# Reverse Proxy & TLS

Kula serves plain HTTP and does not terminate TLS itself. For HTTPS, authentication offloading,
or hosting under a path, run it behind a reverse proxy such as nginx, Apache, Caddy, or
Traefik. This page covers the three things that matter when proxying Kula: **base paths**,
**Unix sockets**, and **the `Secure` cookie / forwarded headers**.

## TLS termination

Terminate TLS at the proxy and forward to Kula over plain HTTP (or a Unix socket). To make
Kula's session cookies set the `Secure` flag and emit HSTS, tell it the upstream is HTTPS:

```yaml
web:
  trust_proxy: true   # honor X-Forwarded-Proto and X-Forwarded-For from the proxy
```

Then have the proxy send `X-Forwarded-Proto: https`. Without `trust_proxy`, Kula ignores that
header (so a client can't spoof it).

> **`X-Forwarded-For`:** Kula uses the **rightmost** (most-trusted) IP in the chain for rate
> limiting and logging. Make sure your proxy appends the real client IP correctly. A
> misconfigured `trust_proxy` can let clients spoof their IP to evade the login limiter.

## The `Host` name, with auth disabled

With `auth.enabled: false` and a TCP listener, Kula answers only `Host` names that no public DNS
server can answer for (IP addresses, `localhost`, single-label and `.local`/`.lan`-style names,
the machine's hostname and `global.hostname`), as protection against DNS rebinding. A proxy that forwards the
browser's `Host` (`proxy_set_header Host $host`, as below) passes your public name through, so
list it:

```yaml
web:
  security:
    allowed_hosts:
      - monitoring.example.com
```

Otherwise every request is answered `403 host not allowed`, and Kula logs the refused name. A
proxy that sends the upstream address (nginx's default `Host $proxy_host`) or connects over a
`unix_socket` needs nothing. See [`web.security`](04-configuration.md#websecurity).

## Base path (sub-path hosting)

To serve Kula under a URL prefix like `https://example.com/kula/`, set:

```yaml
web:
  base_path: "/kula"
```

(or the `KULA_BASE_PATH` environment variable). Every route — UI, API, WebSocket, `/metrics`,
`/health` — is then served under that prefix. Configure your proxy to **forward the prefix
intact** (do *not* strip it). Example nginx:

```nginx
location /kula/ {
    proxy_pass http://127.0.0.1:27960;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    # WebSocket upgrade
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
}
```

## Unix socket listener

Instead of a TCP port, Kula can listen on a Unix domain socket — ideal when the proxy runs on
the same host:

```yaml
web:
  unix_socket: /run/kula/kula.sock
  unix_socket_mode: "0660"   # octal permissions on the socket file
```

When `unix_socket` is set, the TCP listener is **not** opened. Point nginx at the socket:

```nginx
upstream kula { server unix:/run/kula/kula.sock; }
server {
    location / {
        proxy_pass http://kula;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

A tiny stdlib-only test proxy is provided at
[`addons/reverse_proxy.py`](../../addons/reverse_proxy.py) for verifying the Unix socket
listener end-to-end (including WebSocket upgrades) without nginx:

```bash
./addons/reverse_proxy.py --listen 127.0.0.1:8080 --socket /run/kula/kula.sock
```

## Cross-origin / iframe embedding

By default Kula refuses to be framed and rejects cross-origin requests. To embed it elsewhere,
relax the security toggles:

```yaml
web:
  security:
    frame_protection: false      # allow <iframe> embedding
    allowed_origins:
      - https://dashboard.example.com
```

With `allowed_origins` set, CORS headers are sent to matching origins, those origins pass
origin validation, and session cookies switch to `SameSite=None; Secure` (which requires
HTTPS, i.e. TLS or `trust_proxy` + `X-Forwarded-Proto: https`). See the
[Security Model](../dev/08-security.md) for the full behavior.

## Health checks

Point your proxy/load-balancer health checks at:

```
/health
/status
```

Both return `200 OK` with body `kula is healthy`. They remain available even when `web.ui` is
disabled.

Next: [CLI Reference](14-cli-reference.md).
