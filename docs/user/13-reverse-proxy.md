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
  listen: "127.0.0.1"  # only the proxy on this host can connect
  trust_proxy: true    # honor X-Forwarded-Proto and X-Forwarded-For from the proxy
```

Then have the proxy send `X-Forwarded-Proto: https` and `X-Forwarded-For`, as the nginx
examples below do. Without `trust_proxy`, Kula ignores both headers (so a client can't spoof
them).

With `trust_proxy` on, a client that reaches Kula's port directly sends its own
`X-Forwarded-For`: a new address on every request gives it a fresh login rate limit and puts the
address it chose in the log. Keep the listener where only the proxy reaches it: `127.0.0.1`
when the proxy runs on the same host (or a [Unix socket](#unix-socket-listener)), otherwise an
address that a firewall opens to the proxy alone.

> **`X-Forwarded-For`:** Kula uses the **rightmost** (most-trusted) address in the chain, of the
> header's last line, for rate limiting and logging; a value that is not an address counts as
> the proxy's own. The proxy must set the header: one that passes the client's own through
> lets clients pick their IP and evade the login limiter.

## The `Host` name, with auth disabled

With `auth.enabled: false` and a TCP listener, Kula answers only `Host` names that no public DNS
server can answer for (IP addresses, `localhost`, single-label and `.local`/`.lan`-style names,
the machine's hostname and `global.hostname`), as protection against DNS rebinding. A proxy
that forwards the browser's `Host` passes your public name through, so list it. Caddy, Traefik
and Cloudflare Tunnel do so by default; nginx does with `proxy_set_header Host $host`, as below:

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
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
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
  trust_proxy: true          # a socket peer has no address: tell clients apart by X-Forwarded-For
```

When `unix_socket` is set, the TCP listener is **not** opened. Without `trust_proxy` every
client shares one login rate limit, so a few wrong passwords from anyone lock everyone out
(Kula warns about this at startup). Point nginx at the socket:

```nginx
upstream kula { server unix:/run/kula/kula.sock; }
server {
    location / {
        proxy_pass http://kula;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
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
