# Authentication

Authentication is **optional**. When enabled, Kula protects the dashboard and its JSON API
with username/password login, issuing secure session cookies. The health and Prometheus
endpoints are governed separately.

## Enabling auth

### 1. Generate a password hash

```bash
./kula hash-password
```

You'll be prompted for a password (input is masked with asterisks using terminal raw mode).
Kula prints a `password_hash` and `password_salt` computed with **Argon2id**.

The Argon2 parameters used are taken from the `web.auth.argon2` block of your config (defaults:
`time: 3`, `memory: 32768` KiB, `threads: 4` — double the OWASP minimum), so configure those
*before* generating the hash if you want non-default cost.

### 2. Add to config

```yaml
web:
  auth:
    enabled: true
    username: admin
    password_hash: "<paste hash>"
    password_salt: "<paste salt>"
    session_timeout: 24h
    session_max_lifetime: 168h
    argon2:
      time: 3
      memory: 32768
      threads: 4
```

### 3. Restart Kula

You'll now get a login screen before the dashboard loads.

> **Upgrading from 0.21.x or earlier?** Before 0.22.0, `/api/login` accepted any username and
> password while auth was disabled and issued a real session that stayed valid once auth was
> turned on. If an untrusted client could reach Kula while auth was off, stop Kula and delete
> `sessions.json` from the storage directory before enabling auth. Current versions refuse
> login while auth is disabled and clear stored sessions on any run with auth off.

## Multiple users

Add extra accounts under `users`. Each uses the same `argon2` parameters as the primary
account, but has its own hash and salt:

```yaml
web:
  auth:
    enabled: true
    username: admin
    password_hash: "..."
    password_salt: "..."
    users:
      - username: alice
        password_hash: "..."
        password_salt: "..."
      - username: bob
        password_hash: "..."
        password_salt: "..."
```

Generate each user's hash with `./kula hash-password`.

## How sessions work

- On successful login Kula issues a **random session token**. Only the token's **SHA-256
  hash** is stored on disk (`sessions.json`, mode `0600`); the plaintext token lives only in
  the cookie / `Authorization` header.
- Sessions are validated by **token expiry/validity only** — they are *not* bound to client
  IP or User-Agent, so they survive roaming and proxies.
- **Sliding expiration**: each successful request extends the session by `session_timeout`.
- **Absolute lifetime**: a session is refused and deleted once it is older than
  `session_max_lifetime` (default `168h`, 7 days), however recently it was used. Without
  this ceiling, sliding expiration would renew a token forever — an open dashboard tab
  refreshes the session by itself over the WebSocket, so a token copied out of a browser
  profile, a proxy log or a backup of the storage directory would never stop working. Set
  it to `0` to disable the cap and go back to indefinitely renewable sessions.
- **Live connections are re-checked**: the dashboard WebSocket revalidates its session
  every 30 seconds and hangs up once it is gone, so an open tab cannot keep streaming past
  the absolute lifetime (or past a logout). An open dashboard counts as activity for the
  sliding timeout, exactly like any other request. When the server closes an expired
  session, the dashboard clears its readings and shows the login form. Signing in again
  reloads the selected history range without a page refresh.
- A cleanup goroutine purges expired sessions every 5 minutes.
- Sessions are restored from `sessions.json` on restart only for usernames still in the
  config — removing an account from `users` signs it out. A run with auth disabled restores
  none and leaves the file empty, and `/api/login` answers `404` while auth is off.

### Cookie flags

Session cookies are `HttpOnly` and `SameSite=Strict`. The `Secure` flag is set automatically
when the connection is TLS, or when `trust_proxy` is enabled and the proxy sends
`X-Forwarded-Proto: https`. (With `allowed_origins` configured for cross-origin access,
cookies switch to `SameSite=None; Secure`.)

## Bearer token API access

Authenticated API clients can send the session token in the `Authorization` header instead of
a cookie:

```
Authorization: Bearer <session-token>
```

## Brute-force protection

Login is rate-limited over a **5-minute window** by two counters: **5 attempts per
username *and* source IP**, and **15 attempts per source IP** overall. The per-account
counter includes the IP precisely so that a flood of bad passwords aimed at one account
throttles only the caller — it cannot lock that account out for everyone else. Exceeding
either limit temporarily refuses further attempts.

Each attempt costs one Argon2id hash (`argon2.memory`, 32 MiB by default), so Kula also caps
how many hashes run at once across all clients: a quarter of the CPUs, between 1 and 4. A login
that finds them all busy for 2 seconds is refused with `503 Service Unavailable` and
`Retry-After: 1`, which keeps a flood from many addresses from exhausting memory.

> **Reverse proxy caution.** Behind a proxy, configure `trust_proxy` correctly. Kula uses the
> rightmost (most-trusted) IP in `X-Forwarded-For`. A misconfigured `trust_proxy` can let a
> client spoof its IP to evade the login limiter — `kula-scan`'s `BYPASS-XFF` check tests for
> exactly this. See [Reverse Proxy & TLS](13-reverse-proxy.md).

## CSRF protection

When auth is on, state-changing requests require a CSRF synchronizer token (sent in the
`X-CSRF-Token` header) and a matching `Origin`/`Referer`. The dashboard handles this
automatically. See the developer [Security Model](../dev/08-security.md) for details.

Next: [Application Monitoring](08-application-monitoring.md).
