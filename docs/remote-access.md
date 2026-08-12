# Remote access: operate agents from your phone

agent-beacon ships as a local dashboard. This guide covers reaching it **over
the Internet** so you can drive agents from a phone — most usefully to answer a
Claude permission prompt (`waiting` state) while away from your desk.

There are two supported paths:

1. **Built-in Cloudflare quick tunnel** (`--tunnel`) — fastest to try; no proxy
   setup, prints a QR code you scan with your phone.
2. **Self-hosted TLS** (reverse proxy) or **Tailscale** — for a stable,
   long-lived, or private deployment.

The **phone view** (mobile action-first UI) is served the same way as the
desktop dashboard and works over any of these transports. It reuses the same
authenticated session cookie — there is no separate mobile login.

---

## 1. Security first (read this before exposing anything)

Exposing agent-beacon means exposing **interactive control of your agents**. A
visitor who reaches the dashboard can send keystrokes to your Claude sessions.
Therefore:

- **Authentication MUST be enabled.** Run with `--auth=password` (plus
  `--password …` or `--password-hash …`), or `--auth=oidc`. Do not use
  `--auth=none` for any Internet-reachable deployment.
- **`--tunnel` refuses `--auth=none`.** The server exits with an error rather
  than publishing an unauthenticated URL.
- **The mobile view uses the same cookie auth** as the desktop UI. Signing in
  once (password or SSO) covers both.
- **Login is rate-limited per client IP.** After 5 failed attempts the IP is
  locked out with exponential backoff (1s → 5m cap), returning **HTTP 429 +
  `Retry-After`**; even a correct password is refused while locked out. This
  blunts online brute force against the public login endpoint.
- **Browser WebSockets are same-origin only.** The terminal and
  orchestration-events sockets reject a cross-origin handshake (their `Origin`
  must match the server `Host`), so a page a victim visits cannot open a socket
  into their authenticated session (CSRF guard).
- **`Secure` cookies depend on HTTPS reaching the server's view of the
  request.** Behind a reverse proxy that terminates TLS, forward
  `X-Forwarded-Proto: https` so the server marks the session cookie `Secure`
  (see `isTLS()` in `internal/server/server.go`). The built-in tunnel and
  Tailscale serve/funnel present HTTPS to the client already.
- **Quick-tunnel URLs are ephemeral and public** — anyone with the URL reaches
  your login page. Treat the printed `*.trycloudflare.com` URL as sensitive and
  rely on your password/OIDC to gate access.

### Hardening checklist for corporate / work machines

The built-in gate is real but thin, and past the password lies **arbitrary
command execution as you** on the host (file read, exfiltration, `git push`,
internal systems reachable over VPN). Before exposing a corp-provisioned machine,
work through these — roughly in priority order:

1. **Put Cloudflare Access (or your corp IdP) in front of the tunnel** instead of
   a raw quick tunnel. Turns "anyone with the URL" into "an authenticated corp
   identity" — the single biggest risk reduction.
2. **Use a long, random browser password** (e.g. `openssl rand -base64 24`), not
   a memorable one.
3. **Only run the tunnel while actively testing, and tear it down after.** Don't
   leave a public route into your shell open unattended (`Ctrl-C` also kills the
   `cloudflared` child).
4. **Check your corporate acceptable-use / network policy on outbound tunnels
   first.** Routing a work-machine service through Cloudflare may violate policy
   or bypass DLP/egress controls, and EDR may flag `cloudflared` — a compliance
   question independent of any attacker.
5. **Prefer a private mesh (Tailscale, §4) over a public tunnel** where possible —
   identity-based, no public URL at all.
6. **Treat the authenticated phone as a live credential.** The session cookie
   lasts up to 12h with no idle timeout; lock the phone and log out (or stop the
   server) when done.

Generate a bcrypt hash instead of passing a plaintext password on the command
line when possible:

```sh
agent-beacon hash-password        # if available, or use --password once
# then run with:
agent-beacon server --auth=password --password-hash '<bcrypt-hash>' …
```

---

## 2. Option 1 — built-in Cloudflare quick tunnel (`--tunnel`)

A quick tunnel exposes your local server through Cloudflare's edge without any
account, DNS, or inbound firewall changes. It requires the `cloudflared` binary.

### Install `cloudflared`

```sh
# macOS
brew install cloudflared

# Debian/Ubuntu
sudo apt-get install cloudflared      # or download from Cloudflare's releases

# verify
cloudflared --version
```

### Run with auth + tunnel

```sh
agent-beacon server \
  --auth=password --password 'choose-a-strong-password' \
  --tunnel
```

On startup you'll see:

- a `WARN` that a public tunnel is active,
- the assigned URL logged (`public tunnel ready url=https://….trycloudflare.com`),
- and a **QR code + URL printed to stdout**. Scan the QR with your phone camera,
  open the link, sign in with the password, and you're in the phone view.

Options:

- `--tunnel-cmd /path/to/cloudflared` overrides the binary if it isn't on
  `PATH` or you want a specific build.
- `--tunnel-protocol http2|auto` selects the edge transport (default `http2`).
  `cloudflared`'s own default prefers **QUIC** (outbound **UDP/7844**), which
  many corporate networks and VPNs block — a blocked QUIC dial leaves the tunnel
  with no registered edge connection, so visitors get **Cloudflare error
  1033/530**. `http2` registers over TCP/443 and works anywhere DNS+HTTPS do.
  Use `--tunnel-protocol auto` to restore the QUIC-first default on networks
  where UDP/7844 is open.
- The tunnel child is tied to the server's lifetime: **Ctrl-C** (or SIGTERM)
  shuts the server down and kills `cloudflared` — no orphan process.

Notes:

- Quick-tunnel URLs are **regenerated on every run** — they are not stable. For
  a fixed hostname use a named Cloudflare tunnel (an account feature) or Option
  2/3 below.
- If no URL appears within ~20s you'll get a soft `WARN`; the tunnel may still
  be establishing. Check the `cloudflared` output for errors.
- **Wait for the QR/URL before scanning.** Cloudflare error 1033 also appears if
  you open the URL before `cloudflared` has finished registering its edge
  connection (a few seconds after the URL is printed).

---

## 3. Option 2 — reverse proxy with TLS

For a stable hostname you control, terminate TLS at a reverse proxy and forward
to agent-beacon bound to loopback. **You must forward the WebSocket upgrade**
(the terminal stream is a WebSocket) and set `X-Forwarded-Proto: https` so the
session cookie is marked `Secure`.

Bind the server to localhost so only the proxy can reach it:

```sh
agent-beacon server \
  --auth=password --password-hash '<bcrypt-hash>' \
  --address 127.0.0.1:8080
```

### Caddy

Caddy provisions TLS automatically and forwards WebSockets and
`X-Forwarded-Proto` by default:

```caddyfile
beacon.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

### nginx

```nginx
server {
    listen 443 ssl;
    server_name beacon.example.com;

    ssl_certificate     /etc/letsencrypt/live/beacon.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/beacon.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;

        # Required for the terminal WebSocket:
        proxy_http_version 1.1;
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";

        # Required so the server marks the session cookie Secure:
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header Host $host;

        proxy_read_timeout 3600s;   # keep long-lived WS connections open
    }
}
```

---

## 4. Option 3 — Tailscale

If your phone is on your tailnet you can reach the server privately with no
public exposure:

```sh
# private to your tailnet, HTTPS via Tailscale certs
tailscale serve https / http://127.0.0.1:8080
```

To expose it publicly (still gated by agent-beacon auth), use `funnel`:

```sh
tailscale funnel 8080
```

`funnel` publishes to the Internet, so **auth must be on** just as with the
tunnel. Both `serve` and `funnel` present HTTPS to the client, so the `Secure`
cookie works without extra headers.

---

## 5. Option 4 — self-hosted IdP in front of the tunnel

The single biggest risk reduction for a corp machine (hardening checklist item
1) is to stop publishing a raw login page and instead require an **authenticated
identity** before any request reaches agent-beacon. You do **not** need
corporate SSO for this — you can run your own free, open-source Identity Provider
with its **own local user store**, sitting between the Cloudflare tunnel (or
reverse proxy) and agent-beacon.

agent-beacon already exposes two auth modes that a self-hosted IdP plugs into:

- **`--auth=proxy-header`** — agent-beacon trusts an `X-Forwarded-User` header
  set by an upstream that has already authenticated the request (an optional
  allowlist restricts which users are accepted). This is the *forward-auth*
  pattern.
- **`--auth=oidc`** — agent-beacon acts as an OIDC client and redirects
  unauthenticated users to your IdP's authorization endpoint.

> **Trust boundary (critical for `proxy-header`):** the `X-Forwarded-User`
> header is only safe if agent-beacon is **not reachable except through the
> proxy**. Bind it to loopback (`--address 127.0.0.1:8080`) so a client cannot
> connect directly and forge the header. The chain becomes:
> **Cloudflare Tunnel → reverse proxy + IdP forward-auth → agent-beacon on
> 127.0.0.1**.

### Options (all self-hostable, local user store, no corp identity)

| IdP | Integrates via | Notes |
|---|---|---|
| **Authelia** *(recommended)* | `proxy-header` (forward-auth) | Single Go binary; local YAML user DB; built-in TOTP/WebAuthn 2FA and its own per-IP lockout. Lowest friction for a POC. |
| **Pocket ID** | `oidc` | Minimal self-hosted OIDC provider, passkey-only login, local users. Good if you prefer real OIDC over a trusted header. |
| **Authentik** | `oidc` or forward-auth | Full-featured IdP with admin UI; needs Postgres + Redis. |
| **Zitadel / Keycloak** | `oidc` | Enterprise-grade OIDC with own user store; real infra (DB, more RAM). Overkill for a POC. |
| **Dex** | `oidc` | Lightweight OIDC *broker* — usually federates to an upstream (GitHub/LDAP) rather than holding local users. |

### Example — Authelia forward-auth + Caddy + `--auth=proxy-header`

Bind agent-beacon to loopback and trust only the header the proxy sets:

```sh
agent-beacon server \
  --auth=proxy-header --proxy-header-user X-Forwarded-User \
  --address 127.0.0.1:8080
```

Caddy (running the tunnel origin) delegates each request to Authelia, then
forwards the authenticated user:

```caddyfile
beacon.example.com {
    # Ask Authelia to authorize; copy the resolved user into the upstream header.
    forward_auth 127.0.0.1:9091 {
        uri /api/authz/forward-auth
        copy_headers Remote-User>X-Forwarded-User
    }
    reverse_proxy 127.0.0.1:8080
}
```

Authelia holds its own users in `users_database.yml` (no corp directory), so
only accounts you create can pass the forward-auth check — turning "anyone with
the URL" into "an identity you control." Point the Cloudflare tunnel at Caddy
(`--url http://127.0.0.1:443` origin) rather than directly at agent-beacon.

> Even with an IdP in front, keep agent-beacon's own `--auth` enabled as
> defense-in-depth (the per-IP login limiter and same-origin WebSocket guard
> still apply), or use `proxy-header` so the IdP is the sole gate. Do **not**
> combine an external IdP with `--auth=none`.

---

## 6. Using the phone view

- **Entry:** the mobile action-first view activates automatically on narrow
  viewports (≤640px). On any screen you can toggle it with the **Phone /
  Desktop** button in the top bar. Your choice is remembered; if you never
  toggle, the view follows the viewport width.
- **Tap list:** sessions are listed largest-touch-target first, with **waiting
  sessions on top** and a `⚠ Needs input` badge so a Claude prompt is one tap
  away.
- **Action panel:** tap a card to open it. You get:
  - a collapsible **read-only terminal** ("Terminal output") for context, and
  - a sticky **action bar** of big buttons: **Yes / No / Esc**, **1 / 2 / 3**,
    **↑ / ↓ / Enter**, and a **free-text line** (typed text is sent with a
    trailing Enter).
- **State refresh:** the list and the open panel's state pill refresh on the
  same ~2s poll as the desktop, so a `waiting` session clears shortly after you
  answer.

The buttons send discrete keystrokes over the **same** terminal channel the
desktop uses, so they work for Claude's y/n prompts, numbered menus, arrow-key
navigation, and free-form guidance. Because prompt formats vary, the full button
set is exposed — pick whatever the on-screen prompt expects.

---

## 7. Troubleshooting

- **`"cloudflared" not found on PATH`** — install `cloudflared` or pass
  `--tunnel-cmd /path/to/cloudflared`.
- **`--tunnel` exits immediately with an auth error** — you passed
  `--auth=none`. Add `--auth=password` (with `--password`/`--password-hash`) or
  `--auth=oidc`.
- **Phone shows "Cloudflare Tunnel error 1033" (or the URL returns HTTP 530)** —
  the edge has no registered origin connection. The usual cause is a network
  that blocks outbound **QUIC (UDP/7844)**: `cloudflared`'s QUIC dial times out
  (`failed to dial to edge with quic: no recent network activity`) and it never
  registers. The server defaults to `--tunnel-protocol http2` to avoid this; if
  you overrode it with `auto` on such a network, drop the override (or set
  `--tunnel-protocol http2`). You can confirm by running
  `cloudflared tunnel --url http://127.0.0.1:<port>` directly and reading the
  **CONNECTIVITY PRE-CHECKS** block — a `UDP Connectivity … FAIL` there is the
  QUIC block. Also make sure you waited a few seconds for
  `Registered tunnel connection … protocol=http2` before opening the URL.
- **No trycloudflare URL printed** — you'll see a soft `WARN` after ~20s. Check
  the `cloudflared` output above it; the tunnel may still be establishing or
  Cloudflare's edge may be slow.
- **Terminal doesn't stream / WebSocket 400** — your proxy isn't forwarding the
  upgrade. Ensure `Upgrade`/`Connection: upgrade` (nginx) or use Caddy's
  defaults.
- **Login doesn't stick / logged out on every request** — the `Secure` cookie
  isn't being honored because the server didn't see HTTPS. Set
  `X-Forwarded-Proto: https` at the proxy (Caddy/Tailscale do this for you).
- **Ctrl-C leaves a `cloudflared` process** — should not happen (the child is
  tied to the server's shutdown context). If you see one, verify with
  `ps aux | grep cloudflared` and report it.
