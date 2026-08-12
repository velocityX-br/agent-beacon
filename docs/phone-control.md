# Phone Control: action-first mobile view + built-in tunnel

This document is the technical reference and user guide for the **Phone Control**
feature: an action-first mobile UI for driving agents from a phone, plus an
optional built-in Cloudflare quick tunnel for reaching the dashboard over the
Internet.

For the operational "how do I expose this safely" walkthrough (tunnel install,
reverse-proxy TLS, Tailscale), see [`remote-access.md`](./remote-access.md).
This document focuses on **what the feature is, how it works internally, how to
use it, and what has been tested**.

---

## 1. Motivation

agent-beacon is a desktop dashboard: a fixed sidebar plus a full `xterm.js`
terminal panel. That is ideal at a desk but painful on a phone, and until now
the server had no built-in way to be reached over the Internet.

The most time-sensitive event in an agent fleet is a session entering the
**`waiting`** state — a Claude permission prompt or a numbered menu that blocks
progress until a human answers. Phone Control exists so an operator away from
their desk can:

- see at a glance which sessions **need input**,
- read enough terminal context to make a decision, and
- send the answer (`y`/`n`, a menu number, arrow navigation, `Enter`/`Esc`, or a
  free-text line)

…all from a phone, over the Internet, behind the existing authentication.

Explicitly **out of scope**: editing a full terminal on the phone. The mobile
view is read-only for context and sends only discrete keystrokes.

---

## 2. Design principles

| Decision | Choice | Rationale |
|---|---|---|
| Connectivity | Built-in Cloudflare quick tunnel **and** docs for TLS/Tailscale | Fastest path to "works on my phone" without giving up self-hosted options. |
| Mobile UX | **Action-first**: waiting sessions on top, big buttons, read-only context | Optimize for the one-tap answer, not for terminal editing. |
| Auth | **Reuse** existing password / OIDC / proxy-header cookie auth | No new auth surface; the phone shares the desktop's session cookie. |
| Input channel | **Reuse** the existing per-session terminal WebSocket | No new backend endpoint; keystrokes ride the same `{type:"input"}` frame the desktop already sends. |

---

## 3. Architecture

### 3.1 Channel reuse — the key insight

The existing endpoint `/api/v1/terminal/{id}` (a WebSocket handled by
`internal/server/terminal.go`) already does **both** things the mobile view
needs:

1. **Output**: on connect it replays scrollback and then streams live PTY output.
2. **Input**: it accepts JSON frames of the shape `{"type":"input","data":"…"}`
   and forwards the decoded bytes to the PTY via `sess.SendInput`
   (`terminal.go`, `case "input"`).

The mobile view therefore needs **no new backend endpoint**. It opens the same
WebSocket into a *read-only* terminal for context and sends the same
`{type:"input"}` frames for keystrokes.

```
                        ┌───────────────────────────────┐
  Phone (mobile view)   │  agent-beacon server          │
  ┌──────────────────┐  │                               │
  │ mobileTerm        │  │  /api/v1/terminal/{id}  (WS)  │
  │ (read-only xterm) │◄─┼──── scrollback + live output ─┤
  │                   │  │                               │      ┌─────────┐
  │ action buttons    │  │                               │      │  PTY /  │
  │ y n 1 2 3 ↑ ↓ ⏎ ⎋ │──┼──► {type:"input", data} ──────┼────► │ claude  │
  │ free-text line    │  │            SendInput()         │      └─────────┘
  └──────────────────┘  └───────────────────────────────┘
```

### 3.2 Separate socket, shared PTY

The mobile stream uses its **own** WebSocket (`mobileSocket`) and its **own**
read-only terminal (`mobileTerm`), kept entirely separate from the desktop
`attach()` path (`termSocket` / `term`). This means:

- the desktop attach path is untouched — the two can coexist;
- a phone and a desktop can watch the **same** session simultaneously (the
  registry fans output out to all subscribers);
- mobile input hits the same PTY the desktop sees (intended — one operator, one
  agent).

**Critical constraint — no resize from mobile.** The PTY size is negotiated as
the *minimum* across all subscribers. If the phone sent a resize frame it would
shrink a desktop viewer's terminal. So `openMobileContext()` deliberately opens
the socket and streams output but **never** sends a resize (unlike the desktop
`attach()`, which fits and sends dimensions).

### 3.3 Read-only context is a second xterm (not a hand-rolled stripper)

Claude's output uses cursor moves, alt-screen switches, and colour. `xterm.js`
already parses that stream correctly; a `<pre>` + ANSI stripper would garble it.
So the context pane is a second `Terminal` instance created with
`disableStdin:true` (no FitAddon, no `onData`) — it renders identically to the
desktop terminal but cannot be typed into. Keystrokes come only from the action
buttons.

---

## 4. Frontend implementation

All frontend code lives in the existing vanilla-JS SPA (no framework). Files:

- `web/static/index.html` — markup for the toggle and the mobile view.
- `web/static/app.js` — mode state, list/panel rendering, socket wiring.
- `web/static/style.css` — responsive layout and touch-target styling.

### 4.1 Markup (`index.html`)

- **Toggle** in the top bar, hidden until login (`showApp()` unhides it):

  ```html
  <button id="mobile-toggle" class="btn ghost small" hidden>Phone</button>
  ```

- **`#mobile-view`** section (hidden by default), a sibling of the desktop
  `.stage`, containing:
  - `#mobile-list` — the tap list of session cards;
  - `#mobile-action` (hidden until a card is tapped) — a head (`#mobile-back`,
    `#mobile-title`, `#mobile-state` pill), a collapsible
    `<details id="mobile-context">` wrapping the read-only `#mobile-term`, and a
    sticky `.mobile-actionbar` with the button rows plus `#mobile-text-form`.
  - Each keystroke button carries a `data-send` attribute (see §4.4).

### 4.2 Mode state & entry (`app.js`)

Four globals hold mobile state, deliberately separate from the desktop terminal
globals:

```js
let mobileMode   = false; // are we in the phone view?
let mobileTerm   = null;  // the read-only context xterm (lazily created)
let mobileSocket = null;  // its OWN websocket, separate from termSocket
let mobileActiveID = null; // session shown in the action panel, or null
```

- **`applyMobileMode(on, persist)`** — toggles `body.mobile-mode` (so CSS
  re-lays-out) and the `#mobile-view` `hidden` **attribute** (JS-controlled to
  avoid fighting `[hidden]{display:none!important}` in the stylesheet). Entering
  mobile calls `detach()` (frees the desktop socket) then `renderMobileList()`;
  leaving calls `closeMobileAction()`. `persist === false` skips writing
  `localStorage` (used for auto-entry so a viewport-driven default does not
  masquerade as an explicit preference).

- **`initMobileMode()`** — an explicit stored preference
  (`localStorage["agentBeaconMobile"]`) wins; otherwise the viewport width
  (`matchMedia("(max-width: 640px)")`) decides. When there is **no** stored
  preference, a `matchMedia` `change` listener follows later viewport changes
  (rotate/resize); once the user toggles manually, that preference is respected
  and auto-follow stops.

### 4.3 List & panel functions (`app.js`)

- **`renderMobileList(groups)`** — flattens the same visible-managed sessions the
  desktop shows, stable-sorts **waiting-first** (then by the user's saved order),
  and builds large `.mobile-card` buttons showing the workspace label, a state
  pill, the device, and a `⚠ Needs input` badge for waiting sessions. It also
  keeps the open panel's state pill fresh and, **if the active session has
  vanished**, closes the panel with a `[session ended]` notice.

- **`openMobileAction(id, hb)`** — reveals the panel, sets the title/state, and
  calls `openMobileContext(id)`. A lighter analogue of the desktop `attach()`.

- **`ensureMobileTerm()`** — lazily creates the read-only `mobileTerm`
  (`disableStdin:true`, `fontSize:12`, black background); no FitAddon, no
  `onData`.

- **`openMobileContext(id)`** — opens `mobileSocket` to
  `ws(s)://…/api/v1/terminal/{id}`, decodes ArrayBuffer frames with the shared
  `decoder`, writes to `mobileTerm`, scrolls to bottom. **Does not send a
  resize.** On close it prints `[disconnected]` if the panel is still open.

- **`sendMobileInput(bytes)`** — sends `{type:"input", data:bytes}` over
  `mobileSocket` (the same shape `terminal.go` decodes). No-op if the socket is
  not open.

- **`closeMobileAction(notice)`** — optionally writes a notice, closes
  `mobileSocket`, clears `mobileActiveID`, returns to the list; keeps
  `mobileTerm` allocated for reuse.

### 4.4 Keystroke mapping

Single keys are sent **without** a trailing Enter; the free-text line is sent
**with** a trailing `\r`. Named keys are mapped to escape sequences; any other
`data-send` value is sent as its literal character.

| Button | `data-send` | Bytes sent |
|---|---|---|
| Yes | `y` | `"y"` |
| No | `n` | `"n"` |
| 1 / 2 / 3 | `1` / `2` / `3` | `"1"` / `"2"` / `"3"` |
| Up | `up` | `"\x1b[A"` |
| Down | `down` | `"\x1b[B"` |
| Enter | `enter` | `"\r"` |
| Esc | `esc` | `"\x1b"` |
| Free text | (form submit) | `value + "\r"` |

Because prompt formats vary (y/n vs numbered menu vs arrow-select), the full
button set is exposed and the operator picks whatever the on-screen prompt
expects.

### 4.5 Live-state hooks

The feature is almost entirely additive. The only edits to existing function
bodies are:

- one line at the end of `renderDevices()`:
  `if (mobileMode) renderMobileList(groups);` so the tap list refreshes on the
  same ~2s poll as the desktop;
- a branch in `openWaiting()` so the attention toast's **Open** button routes to
  `openMobileAction()` when in mobile mode (else the desktop `attach()`);
- the `detach()` call inside `applyMobileMode()`.

### 4.6 Styling (`style.css`)

Layout is switched **two** ways with identical rules so it works with or without
JS: `@media (max-width: 640px)` (covers a phone that never toggles) and
`body.mobile-mode` (forces the view at any width). In both, `.sidebar` and
`.stage` are hidden and `#mobile-view:not([hidden])` becomes a flex column.

Touch ergonomics:

- `.mobile-card` min-height 64px; waiting cards get a warm `.attention` border
  and the `.mobile-badge` pill.
- `.tapbtn` min-height 48px, 16px font (≥44px touch target), `:active`
  brightness feedback; `.primary` / `.ok` / `.danger` variants.
- `.mobile-actionbar` is sticky at the bottom with
  `padding-bottom: max(8px, env(safe-area-inset-bottom))` so the notch / home
  indicator / on-screen keyboard don't cover the buttons.

---

## 5. Backend implementation — built-in Cloudflare tunnel

The tunnel is **optional** and off by default. When enabled it runs
`cloudflared` as a child process so the local server is reachable at a public
`*.trycloudflare.com` URL without any account, DNS, or inbound-firewall change.

Files:

- `cmd/agent-beacon/server_cmd.go` — flags, auth guard, lifecycle wiring.
- `cmd/agent-beacon/tunnel.go` — `startTunnel()` helper (new file).
- `go.mod` — adds `github.com/mdp/qrterminal/v3` for the QR code.

### 5.1 Flags

```
--tunnel            expose the server via a Cloudflare quick tunnel
                    (requires cloudflared; prints a *.trycloudflare.com URL + QR;
                    refused with --auth=none)
--tunnel-cmd        cloudflared binary path/name used for --tunnel
                    (default "cloudflared")
--tunnel-protocol   cloudflared edge transport: http2 (TCP/443, works behind
                    firewalls that block QUIC) or auto (cloudflared's QUIC-first
                    default) (default "http2")
```

> **Why `http2` is the default.** `cloudflared`'s own default prefers **QUIC**
> (outbound **UDP/7844**), which corporate networks and VPNs frequently block. A
> blocked QUIC dial times out and the tunnel never registers an edge connection,
> so visitors get **Cloudflare error 1033 / HTTP 530**. `http2` registers over
> TCP/443 and works anywhere DNS+HTTPS do. Pass `--tunnel-protocol auto` to
> restore the QUIC-first default where UDP/7844 is open.

### 5.2 Auth guard (fail closed)

Exposing the dashboard grants interactive control of the agents, so the server
**refuses** to start a public tunnel with no browser auth:

```go
if tunnel && auth.Mode(authMode) == auth.ModeNone {
    return fmt.Errorf("--tunnel refuses --auth=none: exposing the server " +
        "publicly without auth would grant anyone control; use " +
        "--auth=password/--password (or oidc)")
}
```

Even with auth on, startup logs a `WARN` that a public tunnel is active and the
URL should be treated as sensitive.

### 5.3 `startTunnel()`

`startTunnel(ctx, bin, port, protocol, log) (*exec.Cmd, error)`:

- runs `exec.CommandContext(ctx, bin, "tunnel", "--url",
  "http://127.0.0.1:<port>")`, appending `--protocol <protocol>` unless
  `protocol` is empty or `"auto"` — the child is tied to the server's
  `signal.NotifyContext`, so **Ctrl-C / SIGTERM kills `cloudflared` too** (no
  orphan);
- captures `StderrPipe()` and scans it with a `bufio.Scanner` for the regex
  `https://[a-z0-9-]+\.trycloudflare\.com`;
- on the **first** match: logs `public tunnel ready`, prints a banner, renders a
  scannable QR via `qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)`,
  and prints the URL — then keeps draining stderr so the pipe buffer never fills;
- a missing binary yields a clear `"cloudflared" not found on PATH` error; if no
  URL appears within ~20s it logs a soft `WARN` (the tunnel may still be
  establishing) and keeps running.

### 5.4 Lifecycle

The tunnel launches after the HTTP listener goroutine and before the shutdown
`select`. Its `cmd.Wait()` is reaped in a goroutine so an early `cloudflared`
exit is logged (not treated as fatal to the server). On shutdown the child is
`Kill()`ed if it was started.

### 5.5 Why a subprocess (not an embedded library)

Shelling out to `cloudflared` keeps the binary light and matches the optional,
gated nature of `--tunnel` — users who never expose the server don't carry the
tunnel stack.

---

## 6. Security model

Exposing the dashboard publicly turns a `localhost`-only tool into an
Internet-reachable route **into a live shell on the host** — the terminal
WebSocket relays raw keystrokes straight into the PTY (`sess.SendInput`). The
browser password is the primary gate in front of that, so the login and socket
surfaces are hardened as follows.

### 6.1 Built-in protections

- **Auth is mandatory for any Internet exposure.** `--tunnel` hard-refuses
  `--auth=none`. The mobile view uses the same session cookie as the desktop —
  sign in once (password or SSO) and both work.
- **Password is bcrypt-checked**, never compared in plaintext
  (`internal/auth/password.go`).
- **Login is rate-limited per client IP.** After 5 failed attempts an IP is
  locked out with an **exponential backoff** (1s doubling up to a 5m cap; idle
  IPs are forgotten after 15m). While locked out the endpoint returns
  **HTTP 429** with a `Retry-After` header — even a correct password is refused
  until the window elapses. This blunts online brute force against the public
  login. See `loginLimiter` in `internal/server/loginlimit.go`, wired into
  `handleLogin` in `internal/server/server.go`. The throttle key is the
  left-most `X-Forwarded-For` entry (set by the tunnel/proxy) else the transport
  remote address; it is used only for bucketing, never for authorization, so a
  forged header can at worst rotate the attacker's own bucket — it cannot bypass
  auth.
- **Browser WebSockets enforce a same-origin policy.** The terminal and
  orchestration-events sockets are accepted with `OriginPatterns` pinned to the
  request's own `Host` (`browserWSOptions()` in `internal/server/terminal.go`).
  A page a victim visits therefore cannot open a socket into their
  authenticated session — the browser sends its real `Origin`, which won't match
  the server `Host`, and the handshake is rejected with HTTP 403. This is the
  CSRF guard for the sockets. (The agent↔server PSK socket is not browser-facing
  and is unaffected.)
- **Quick-tunnel URLs are ephemeral and public.** Anyone with the URL reaches
  your login page; the password/OIDC gate is what protects you. The URL changes
  on every run and is **not a secret** (it is printed to your terminal and to
  cloudflared's logs).
- **`Secure`, `HttpOnly`, `SameSite=Lax` session cookie** with a 32-byte random
  opaque token. `Secure` depends on the server seeing HTTPS: the built-in tunnel
  and Tailscale serve/funnel present HTTPS already; behind a reverse proxy that
  terminates TLS you must forward `X-Forwarded-Proto: https` (honored by
  `isTLS()` in `internal/server/server.go`) or the cookie won't stick.
- **The tunnel child dies with the server** — it's bound to the shutdown context.

### 6.2 Recommended hardening for corporate environments

The built-in gate is real but thin. Driving a **corp-provisioned machine** over
a public tunnel meaningfully raises risk: past the password lies arbitrary
command execution as you (file read, exfiltration, `git push`, reaching internal
systems your machine can see over VPN). Before relying on this on a work device,
apply these — roughly in priority order:

1. **Put Cloudflare Access (or your corp IdP) in front of the tunnel** instead
   of a raw quick tunnel. This turns "anyone with the URL" into "an
   authenticated corp identity" and is the single biggest risk reduction. No
   corporate SSO? You can run a free, self-hosted IdP with its own local user
   store (e.g. Authelia forward-auth → `--auth=proxy-header`, or Pocket ID →
   `--auth=oidc`) — see
   [remote-access.md §5 "Option 4 — self-hosted IdP"](./remote-access.md#5-option-4--self-hosted-idp-in-front-of-the-tunnel).
2. **Use a long, random browser password** (e.g. `openssl rand -base64 24`), not
   a memorable one. The rate-limiter slows guessing but a weak secret on a public
   endpoint is still the weakest link.
3. **Only run the tunnel while you're actively testing, and tear it down after.**
   Don't leave a public route into your shell open unattended. The tunnel child
   already dies with the server, so `Ctrl-C` is sufficient.
4. **Check your corporate acceptable-use / network policy on outbound tunnels
   *before* using this on the work PC.** Routing a work-machine service through a
   third party (Cloudflare) may itself violate policy or bypass DLP/egress
   controls, and EDR may flag `cloudflared` — this is a compliance question,
   independent of whether anyone attacks you.
5. **Prefer a private mesh (Tailscale) over a public tunnel** where possible.
   Tailscale keeps the surface on an identity-based private network with no
   public URL at all; see `remote-access.md`.
6. **Treat the authenticated phone as a live credential.** The session cookie
   lasts up to 12h with no idle timeout, so a scanned-once phone stays signed in
   for the workday — lock the phone, and log out (or stop the server) when done.

---

## 7. User guide

### 7.1 Reach the dashboard from your phone (quickest path)

1. Install `cloudflared` (`brew install cloudflared`, or your distro's package /
   Cloudflare's release).
2. Run the server with auth **and** the tunnel:

   ```sh
   agent-beacon server \
     --auth=password --password 'choose-a-strong-password' \
     --tunnel
   ```

3. On startup you'll see a `WARN` about the public tunnel and a **QR code + URL**
   printed to stdout. Scan the QR with your phone camera, open the link, sign in,
   and you're in the phone view.

> **If the phone shows "Cloudflare Tunnel error 1033":** your network is likely
> blocking QUIC (outbound UDP/7844). The server defaults to
> `--tunnel-protocol http2` to avoid this; if you overrode it with `auto`, drop
> the override. Also wait a couple of seconds after the URL prints (until
> `cloudflared` logs `Registered tunnel connection`) before opening it. See the
> Troubleshooting section of [`remote-access.md`](./remote-access.md).

For a stable hostname (reverse-proxy TLS) or a private tailnet
(Tailscale), see [`remote-access.md`](./remote-access.md).

### 7.2 Entering / leaving the phone view

- The phone view activates **automatically** on narrow viewports (≤640px).
- On any screen, the **Phone / Desktop** button in the top bar toggles it. Your
  choice is remembered; if you never toggle, the view follows the viewport width.

### 7.3 Answering a waiting session

1. The list shows sessions **largest-touch-target first**, with **waiting
   sessions on top** and a `⚠ Needs input` badge.
2. **Tap a card** to open its action panel. You get:
   - a collapsible **read-only terminal** ("Terminal output") for context, and
   - a sticky **action bar**: **Yes / No / Esc**, **1 / 2 / 3**, **↑ / ↓ /
     Enter**, and a **free-text line** (sent with a trailing Enter).
3. Tap the button the prompt expects (e.g. `y` for a yes/no, `1` for a menu, or
   type a line and **Send** for free-form guidance).
4. The panel's state pill and the list refresh on the same ~2s poll, so a
   `waiting` session clears shortly after you answer. Tap **‹ Back** to return to
   the list.

### 7.4 Notes & limitations

- The on-screen keyboard may briefly cover the sticky action bar; the safe-area
  padding and input focus-scroll mitigate this. Acceptable for the POC.
- The mobile context terminal is **read-only** by design — use the buttons and
  the free-text line to send input, not the terminal itself.
- Prompt formats vary, so the full button set is exposed; pick what the prompt
  shows.

---

## 8. Completed test cases

The following were executed against a local build during implementation.

### 8.1 Build & static analysis

| # | Test | Result |
|---|---|---|
| T1 | `go build ./...` | ✅ Pass (clean) |
| T2 | `go vet ./...` | ✅ Pass (clean) |
| T3 | `go get github.com/mdp/qrterminal/v3 && go mod tidy` | ✅ Dependency added (`v3.2.1`, `rsc.io/qr` transitive) |

### 8.2 Tunnel backend

| # | Test | Result |
|---|---|---|
| T4 | `server --auth=none --tunnel` | ✅ Hard error refusing `--auth=none` (fails closed before listening) |
| T5 | `server --auth=password --password test --tunnel` with `cloudflared` **absent** | ✅ Clear `"cloudflared" not found on PATH` error pointing to `--tunnel-cmd` / docs |
| T6 | Ctrl-C / stop with tunnel path exercised | ✅ No orphan `cloudflared` process (`ps aux \| grep cloudflared` → none); child is bound to the shutdown context |
| T7 | **Real happy path**: `server --auth=password --password 'StrongPwd@' --address 127.0.0.1:18443 --tunnel` with `cloudflared` present (default `--tunnel-protocol http2`) | ✅ Banner + `*.trycloudflare.com` URL + QR rendered to stdout in ~6s; **public URL returned HTTP 200** for both `/` (login page) and `/api/v1/health` |
| T8 | Port-conflict diagnosis: `--tunnel` on default `:8080` while `kubectl port-forward` also held `127.0.0.1:8080` | ✅ Reproduced Cloudflare **error 1033** — cloudflared forwarded to the wrong origin (kubectl answered `/health` → 404); resolved by binding a free loopback port (`--address 127.0.0.1:18443`) |
| T9 | QUIC-block diagnosis: default protocol on a network blocking outbound UDP/7844 | ✅ Reproduced **error 1033 / HTTP 530** (`cloudflared` logged `Failed to dial a quic connection: timeout` + `UDP Connectivity FAIL`); `--protocol http2` registered over TCP/443 (`Registered tunnel connection … protocol=http2`) and public health returned HTTP 200 |
| T10 | New flag default: `--tunnel-protocol` defaults to `http2` | ✅ Confirmed the http2 default makes the common QUIC-blocked corporate/VPN case work out of the box; `--tunnel-protocol auto` restores cloudflared's QUIC-first default |

### 8.3 Mobile view (desktop-simulated + Playwright at 390×844)

Verified against a local server (`--auth=password --password test`) with a
PTY-wrapped session used as the interactive target.

| # | Test | Result |
|---|---|---|
| T11 | Auto-entry: viewport ≤640px shows `#mobile-view`, hides `.sidebar` / `.stage` | ✅ Mobile layout confirmed via DOM inspection |
| T12 | Toggle persistence: Phone/Desktop button flips the view and persists to `localStorage["agentBeaconMobile"]` | ✅ Persisted; label updates Phone↔Desktop |
| T13 | Login: password `test` succeeds, dashboard renders | ✅ Sessions dashboard shown after login |
| T14 | Waiting-first list: a session in `waiting` shows a `.mobile-card` with `⚠ Needs input` | ✅ Card rendered with badge, "Needs Input" state |
| T15 | Open action panel: tap card → `#mobile-action` shows, read-only `#mobile-term` streams live output | ✅ Live output rendered into the read-only terminal |
| T16 | Free-text send: type `hello-from-phone` + **Send** → PTY echoes it back | ✅ Text appeared in `#mobile-term`; input dispatched and cleared |
| T17 | Quick-action buttons: `data-send` buttons dispatch keystrokes → PTY reacts | ✅ Button inputs appeared in terminal output, confirming the `{type:"input"}` relay |
| T18 | State transition: after interaction the session pill moves `waiting` → `running` | ✅ State cleared "Needs input" within the ~2s poll |
| T19 | List/action navigation: show/hide toggle between `#mobile-list` and `#mobile-action`; **‹ Back** returns | ✅ Navigation works; back returns to the list |
| T20 | Network assertion: `{type:"input", …}` frames sent over `/api/v1/terminal/{id}` | ✅ Confirmed via network inspection |
| T21 | Vanished-session path: with the panel open, the target session ages out of the registry | ✅ After the session left the registry (`GET /api/v1/sessions` → `[]`), `renderMobileList` no longer finds `mobileActiveID` and closes the panel with `[session ended]` |

### 8.4 Security hardening (Go unit + integration tests)

Run with `go test ./internal/server/ -run 'TestLoginLimiter|TestClientIP|TestLoginRateLimitOverHTTP|TestTerminalWSRejectsCrossOrigin'`.

| # | Test | Result |
|---|---|---|
| T22 | `TestLoginLimiterLockout` — allow up to threshold, lock out after, unlock when backoff elapses, reset on success | ✅ Pass (fake clock) |
| T23 | `TestLoginLimiterBackoffGrows` — backoff grows with repeated failures, capped at `maxBackoff` | ✅ Pass |
| T24 | `TestLoginLimiterWindowForget` — an idle IP's state is reaped after `window` | ✅ Pass |
| T25 | `TestClientIP` — throttle key = left-most `X-Forwarded-For` else `RemoteAddr` host (chain/spacing/no-port cases) | ✅ Pass |
| T26 | `TestLoginRateLimitOverHTTP` — after `threshold` bad logins the endpoint returns **429 + `Retry-After`**; a correct password is refused while locked out | ✅ Pass |
| T27 | `TestTerminalWSRejectsCrossOrigin` — a forged `Origin` handshake to `/api/v1/terminal/{id}` is rejected (HTTP 403); same-origin is accepted | ✅ Pass (`Origin "evil.example.com" is not authorized for Host …`) |
| T28 | Full suite `go test ./...` + `go vet ./...` after the changes | ✅ All packages pass; vet clean |

### 8.5 Not yet covered (follow-ups)

- **Live reverse-proxy TLS** (Caddy / nginx) end-to-end: documented in
  `remote-access.md` but not executed in this cycle — worth validating that the
  wss upgrade works and the `Secure` cookie sticks behind a real proxy.
- **On-device phone drive**: the public tunnel serves HTTP 200 and the mobile
  view is verified at 390×844, but a full end-to-end tap-through *from a physical
  phone* over the live tunnel is the remaining manual check.
- **iOS/Android on-device keyboard behavior**: the safe-area padding is in place
  but only simulated at 390×844; on-device keyboard overlap is a known POC risk.

---

## 9. File map

| File | Role |
|---|---|
| `web/static/index.html` | `#mobile-toggle` in the top bar; `#mobile-view` (list + action panel) markup. |
| `web/static/app.js` | 4 mobile globals; `applyMobileMode` / `initMobileMode`; `renderMobileList`, `openMobileAction`, `ensureMobileTerm`, `openMobileContext`, `sendMobileInput`, `closeMobileAction`; hooks in `renderDevices` / `openWaiting`; wiring block. |
| `web/static/style.css` | `@media (max-width:640px)` + `body.mobile-mode` layout; `.mobile-card`, `.mobile-badge`, `.mobile-actionbar`, `.tapbtn`. |
| `cmd/agent-beacon/server_cmd.go` | `--tunnel` / `--tunnel-cmd` / `--tunnel-protocol` flags; auth guard; tunnel launch + shutdown kill. |
| `cmd/agent-beacon/tunnel.go` | `startTunnel()` — runs `cloudflared`, scans for the URL, prints QR. |
| `go.mod` / `go.sum` | `github.com/mdp/qrterminal/v3` dependency. |
| `internal/server/terminal.go` | The reused `{type:"input"}` → `SendInput` channel; `browserWSOptions()` same-origin guard for browser WebSockets. |
| `internal/server/loginlimit.go` | `loginLimiter` — per-IP login rate-limiting with exponential-backoff lockout; `clientIP()`. |
| `internal/server/server.go` | `handleLogin` wiring: lockout check → 429/`Retry-After`, record failure/success; bcrypt verify. |
| `internal/server/loginlimit_test.go` / `origin_test.go` | Unit + integration tests for the two mitigations. |
| `docs/remote-access.md` | Operational guide: tunnel install, reverse-proxy TLS, Tailscale, security, troubleshooting. |
