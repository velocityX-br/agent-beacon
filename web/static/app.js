// agent-beacon dashboard SPA — original vanilla JS.
// Flow: fetch /api/v1/login-info to decide the auth UX, gate on /api/v1/sessions
// (401 => show login), poll sessions, and attach an xterm.js terminal over a
// per-session WebSocket at /api/v1/terminal/{id}.

(() => {
  "use strict";

  const el = (id) => document.getElementById(id);
  const loginView = el("login-view");
  const appView = el("app-view");
  const connStatus = el("conn-status");

  // ---- Per-session remarks --------------------------------------------------
  // User-authored notes that label a session with its task/goal, so the sidebar
  // card is quickly identifiable at a glance. Stored client-side in
  // localStorage keyed by session id (sessions are in-memory server-side, so
  // there is nothing durable to hang a note on server-side). A single JSON
  // object maps sessionID -> remark string.
  const REMARKS_KEY = "agentBeaconRemarks";
  // While a remark input is open we must NOT let the 2s poll re-render the
  // device list, because rebuilding the DOM destroys the <input> and fires its
  // blur handler, closing the edit mid-typing. This holds the session id being
  // edited (or null). renderDevices() skips its wholesale re-render while set,
  // so the user can type freely and finish by clicking outside the input.
  let editingRemarkID = null;
  // Set to the session id currently being drag-reordered (or null). Like
  // editingRemarkID, renderDevices() skips its wholesale re-render while a drag
  // is in flight so the 2s poll can't destroy the card mid-drag and abort it.
  let draggingID = null;
  function loadRemarks() {
    try {
      const raw = localStorage.getItem(REMARKS_KEY);
      const obj = raw ? JSON.parse(raw) : {};
      return (obj && typeof obj === "object") ? obj : {};
    } catch {
      return {};
    }
  }
  function getRemark(id) {
    if (!id) return "";
    const v = loadRemarks()[id];
    return typeof v === "string" ? v : "";
  }
  function setRemark(id, text) {
    if (!id) return;
    const all = loadRemarks();
    const t = (text || "").trim();
    if (t) all[id] = t; else delete all[id];
    try {
      localStorage.setItem(REMARKS_KEY, JSON.stringify(all));
    } catch { /* quota / disabled storage: ignore */ }
  }

  // ---- Per-session ordering -------------------------------------------------
  // Users can move session cards up/down to arrange them however they like.
  // Sessions are ephemeral server-side and the server returns them in its own
  // order, so — like remarks — the desired order is persisted client-side in
  // localStorage as a single JSON map of sessionID -> integer rank. Lower rank
  // sorts first. Sessions without a stored rank keep the server's relative
  // order (rank defaults to +Infinity) and appear after ranked ones.
  const ORDER_KEY = "agentBeaconSessionOrder";
  function loadOrder() {
    try {
      const raw = localStorage.getItem(ORDER_KEY);
      const obj = raw ? JSON.parse(raw) : {};
      return (obj && typeof obj === "object") ? obj : {};
    } catch {
      return {};
    }
  }
  function saveOrder(map) {
    try {
      localStorage.setItem(ORDER_KEY, JSON.stringify(map));
    } catch { /* quota / disabled storage: ignore */ }
  }
  // rankOf returns the stored rank for an id, or +Infinity when unranked so
  // unranked sessions fall to the end while preserving their incoming order.
  function rankOf(order, id) {
    const v = order[id];
    return (typeof v === "number" && isFinite(v)) ? v : Infinity;
  }
  // sortSessionsByOrder returns a stable copy of sessions sorted by stored
  // rank. Array.prototype.sort is stable (ES2019+), so equal-rank/unranked
  // sessions retain the server's relative order.
  function sortSessionsByOrder(sessions) {
    const order = loadOrder();
    return sessions
      .map((s, i) => ({ s, i }))
      .sort((a, b) => {
        const ra = rankOf(order, a.s.id);
        const rb = rankOf(order, b.s.id);
        if (ra !== rb) return ra - rb;
        return a.i - b.i; // tie-break: preserve incoming order
      })
      .map((x) => x.s);
  }
  // reorderByDrop moves the dragged session to a new slot in its device group.
  // `sessions` is the group's current display order; `from` is the dragged
  // card's index; `to` is the target slot (0..len, where len means "end").
  // The group's display order is spliced, then every id is re-ranked to its
  // new position so the stored order is well-defined for the whole group.
  function reorderByDrop(sessions, from, to) {
    if (from === to || from === to - 1) return; // no-op: dropped in place
    const ids = sessions.map((s) => s.id);
    const [moved] = ids.splice(from, 1);
    // Splicing out `from` shifts every later index down by one, so a target
    // that was after the source must be decremented to land in the right slot.
    const insertAt = to > from ? to - 1 : to;
    ids.splice(insertAt, 0, moved);
    const order = loadOrder();
    ids.forEach((id, i) => { order[id] = i; });
    saveOrder(order);
    if (lastGroups) renderDevices(lastGroups);
  }

  let provider = "password";
  let pollTimer = null;
  let activeSessionID = null;
  // Latest device/session snapshot from the last poll. Retained so client-side
  // actions (e.g. reordering session cards) can trigger a re-render without
  // waiting for the next poll.
  let lastGroups = null;
  let term = null;
  let fitAddon = null;
  let termSocket = null;
  let decoder = new TextDecoder();
  // ResizeObserver on the terminal host so the xterm re-fits on ANY container
  // size change (sidebar re-render, panel toggle, pill text growth), not only
  // on window.resize. A short debounce coalesces bursts (window drags, the 2s
  // poll re-render) into a single fit so we don't flood the server with resize
  // frames or flicker through intermediate widths.
  let termResizeObserver = null;
  let fitDebounceTimer = null;
  // On (re)attach we pin the terminal viewport to the newest output so a long
  // backlog doesn't leave the user scrolled up in history. We stay pinned only
  // until the user manually scrolls up, then respect their position.
  let stickToBottom = true;

  // ---- Mobile action-first view ---------------------------------------------
  // A touch-optimized layout for operating agents from a phone. It reuses the
  // SAME per-session terminal WebSocket as the desktop stage — both to stream
  // read-only context (fed into `mobileTerm`, a stdin-disabled xterm) and to
  // send keystrokes ({type:"input"}, decoded by terminal.go into SendInput).
  // `mobileSocket` is deliberately SEPARATE from `termSocket` so the desktop
  // attach() path is untouched and the two can coexist via the registry's
  // per-session fan-out. `mobileActiveID` is the session currently shown in the
  // action panel (or null when the tap list is showing).
  let mobileMode = false;
  let mobileTerm = null;
  let mobileSocket = null;
  let mobileActiveID = null;

  // ---- Intervention alerts --------------------------------------------------
  // Sessions in the "waiting" state have signalled (via Claude's Notification
  // hook) that Claude is blocked on the user. We track which ids we've already
  // alerted for so a browser Notification fires once per transition into
  // waiting, not on every 2s poll. The base document title is restored when no
  // session needs attention.
  const alertedIDs = new Set();
  const baseTitle = document.title;
  let notifyPermission = (typeof Notification !== "undefined") ? Notification.permission : "denied";

  // ---- Auth / bootstrapping -------------------------------------------------

  async function loginInfo() {
    try {
      const r = await fetch("/api/v1/login-info");
      return await r.json();
    } catch {
      return { provider: "password", provider_name: "", start_url: "", logout_url: "/logout" };
    }
  }

  async function sessionsProbe() {
    // Returns {ok, data} — ok=false with status 401 means we need to log in.
    const r = await fetch("/api/v1/sessions", { credentials: "same-origin" });
    if (r.status === 401) return { ok: false, status: 401 };
    if (!r.ok) return { ok: false, status: r.status };
    return { ok: true, data: await r.json() };
  }

  function showLogin(info) {
    appView.hidden = true;
    loginView.hidden = false;
    el("logout-btn").hidden = true;

    const hint = el("login-hint");
    const pw = el("password");
    const sso = el("sso-link");
    const form = el("login-form");

    if (provider === "oidc" && info.start_url) {
      hint.textContent = "Sign in with " + (info.provider_name || "SSO") + ".";
      pw.hidden = true;
      form.querySelector('button[type="submit"]').hidden = true;
      sso.hidden = false;
      sso.href = info.start_url;
    } else {
      hint.textContent = "Enter the server password to continue.";
      pw.hidden = false;
      sso.hidden = true;
    }
  }

  function showApp() {
    loginView.hidden = true;
    appView.hidden = false;
    el("logout-btn").hidden = provider === "none";
    el("mobile-toggle").hidden = false;
  }

  // ---- Mobile mode entry/toggle ---------------------------------------------
  const MOBILE_KEY = "agentBeaconMobile";
  const mobileMedia =
    (typeof matchMedia === "function") ? matchMedia("(max-width: 640px)") : null;

  // applyMobileMode switches between the desktop stage and the phone view. It
  // toggles the body class (so CSS re-lays-out) and the #mobile-view hidden
  // attribute (JS-controlled to avoid fighting [hidden]{display:none!important}
  // at style.css:22), persists the choice, and updates the toggle label.
  // Entering mobile detaches the desktop terminal (frees its socket) and paints
  // the tap list; leaving closes any open action panel. `persist` is false only
  // for auto-entry from the media query, so a viewport-driven choice does not
  // masquerade as an explicit user preference (which would stop auto-follow).
  function applyMobileMode(on, persist) {
    mobileMode = !!on;
    document.body.classList.toggle("mobile-mode", mobileMode);
    el("mobile-view").hidden = !mobileMode;
    if (persist !== false) {
      try { localStorage.setItem(MOBILE_KEY, mobileMode ? "1" : "0"); } catch { /* ignore */ }
    }
    el("mobile-toggle").textContent = mobileMode ? "Desktop" : "Phone";
    if (mobileMode) {
      detach();
      renderMobileList(lastGroups);
    } else {
      closeMobileAction();
    }
  }

  // initMobileMode picks the initial view: an explicit stored preference wins,
  // else the viewport width (<=640px => phone). When there is NO stored
  // preference we follow later viewport changes (rotate/resize); once the user
  // has toggled manually we respect that and stop auto-following.
  function initMobileMode() {
    let stored = null;
    try { stored = localStorage.getItem(MOBILE_KEY); } catch { /* ignore */ }
    const hasPref = stored === "1" || stored === "0";
    const initial = hasPref ? stored === "1" : (mobileMedia ? mobileMedia.matches : false);
    applyMobileMode(initial, hasPref);
    if (mobileMedia && typeof mobileMedia.addEventListener === "function") {
      mobileMedia.addEventListener("change", (e) => {
        let s = null;
        try { s = localStorage.getItem(MOBILE_KEY); } catch { /* ignore */ }
        if (s !== "1" && s !== "0") applyMobileMode(e.matches, false);
      });
    }
  }

  async function doPasswordLogin(ev) {
    ev.preventDefault();
    const errBox = el("login-error");
    errBox.hidden = true;
    const password = el("password").value;
    try {
      const r = await fetch("/api/v1/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "same-origin",
        body: JSON.stringify({ password }),
      });
      if (!r.ok) {
        errBox.textContent = "Incorrect password.";
        errBox.hidden = false;
        return;
      }
      await start();
    } catch {
      errBox.textContent = "Login failed. Is the server reachable?";
      errBox.hidden = false;
    }
  }

  async function doLogout() {
    try {
      await fetch("/api/v1/logout", { method: "POST", credentials: "same-origin" });
    } catch { /* ignore */ }
    stopPolling();
    detach();
    location.reload();
  }

  // ---- Session list ---------------------------------------------------------

  function stateClass(s) {
    return "state " + (s || "idle");
  }

  // workspaceName returns the folder name (basename of the heartbeat cwd) to use
  // as a session's primary label. The wrapped command is almost always "claude"
  // and thus useless as a per-session index, so the working directory's leaf
  // name (e.g. "slack-mcp-server") identifies the workspace instead. Returns ""
  // when there is no cwd so callers can fall back to command/id.
  function workspaceName(hb) {
    const cwd = (hb && hb.cwd) || "";
    if (!cwd) return "";
    const parts = cwd.replace(/[/\\]+$/, "").split(/[/\\]/);
    return parts[parts.length - 1] || "";
  }

  function renderDevices(groups) {
    // Retain the latest snapshot so client-side actions (reordering) can
    // re-render without waiting for the next poll.
    lastGroups = groups;
    // A remark edit OR a drag-reorder is in progress: skip the destructive full
    // re-render so the open <input> keeps focus / the drag isn't aborted by the
    // card being rebuilt underneath the pointer. The next poll after the action
    // commits refreshes the list normally. We still run attention processing so
    // the sidebar banner/title stay current.
    if (editingRemarkID !== null || draggingID !== null) {
      processAttention(groups);
      return;
    }
    const list = el("device-list");
    list.innerHTML = "";
    if (!groups || groups.length === 0) {
      const p = document.createElement("p");
      p.className = "muted";
      p.style.padding = "10px";
      p.textContent = "No sessions yet. Run `agent-beacon monitor` to discover running Claude agents, or `agent-beacon session -- claude` for an interactive one.";
      list.appendChild(p);
      // Still process attention so the banner/title/alerts clear when the last
      // (possibly waiting) session disappears.
      processAttention(groups);
      return;
    }
    for (const g of groups) {
      const wrap = document.createElement("div");
      wrap.className = "device-group";
      const name = document.createElement("div");
      name.className = "device-name";
      const label = document.createElement("span");
      label.textContent = g.device;
      name.appendChild(label);
      wrap.appendChild(name);

      // Prominent, always-present per-device action to open a NEW managed
      // session. Sessions are only created on explicit request — this is that
      // request. Disabled with a hint when the device announced no projects.
      const hasProjects = g.projects && g.projects.length > 0;
      const openBtn = document.createElement("button");
      openBtn.className = "btn primary small open-session";
      openBtn.textContent = "+ Open Session";
      if (hasProjects) {
        openBtn.title = "Open a new interactive session on " + g.device;
        openBtn.addEventListener("click", (e) => {
          e.stopPropagation();
          openSpawn(g.device, g.projects);
        });
      } else {
        openBtn.disabled = true;
        openBtn.title = "No projects announced. Set AGENT_BEACON_PROJECTS_DIR on " +
          g.device + " (or run `agent-beacon session -- claude` there).";
      }
      wrap.appendChild(openBtn);

      // Only managed (interactive) sessions are shown; observed processes are
      // hidden. Compute the visible list first, then apply the user's stored
      // ordering, so the ▲/▼ move controls operate on exactly the cards on
      // screen and first/last can be detected for button disabling.
      const visible = sortSessionsByOrder(
        g.sessions.filter((s) => !(s.kind === "observed" || s.attachable === false))
      );
      for (let vi = 0; vi < visible.length; vi++) {
        const s = visible[vi];
        const hb = s.heartbeat || {};
        const waiting = s.state === "waiting";
        const card = document.createElement("div");
        card.className = "session-card"
          + (waiting ? " attention" : "")
          + (s.id === activeSessionID ? " active" : "");
        card.dataset.id = s.id;
        // Enable HTML5 drag-and-drop reordering. Only a drag started from the
        // handle (⠿) should move the card, so the card itself is draggable only
        // while the pointer is on the handle (see the handle's mousedown/up).
        wireCardDrag(card, visible, vi);

        // Drag handle: the sole grab affordance for reorder. Living in its own
        // column keeps the card body free for click-to-attach and the inline
        // remark editor without ambiguity. It toggles the card's draggable flag
        // on press so a plain click/drag elsewhere never initiates a reorder.
        const handle = document.createElement("span");
        handle.className = "drag-handle";
        handle.textContent = "⠿";
        handle.title = "Drag to reorder";
        handle.addEventListener("mousedown", () => { card.draggable = true; });
        handle.addEventListener("mouseup", () => { card.draggable = false; });
        handle.addEventListener("click", (e) => e.stopPropagation());
        card.appendChild(handle);

        const row1 = document.createElement("div");
        row1.className = "row1";
        // Primary label: the workspace/folder name (basename of cwd) so each
        // session is identifiable at a glance. The wrapped command is almost
        // always "claude", so it makes a poor per-session index; fall back to it
        // (then the session id) only when there is no cwd.
        const cmd = document.createElement("span");
        cmd.className = "cmd";
        cmd.textContent = workspaceName(hb) || hb.command || s.id;
        if (hb.cwd) cmd.title = hb.cwd;
        row1.appendChild(cmd);
        card.appendChild(row1);

        // Row 2 sits directly under the workspace index and shows the session's
        // current state as a colored pill. When the session is blocked on the
        // user (waiting), append an explicit call-to-action so the card itself —
        // not just the sidebar banner — prompts the operator to respond.
        const row2 = document.createElement("div");
        row2.className = "row2";
        const st = document.createElement("span");
        st.className = stateClass(s.state);
        st.textContent = s.state || "idle";
        row2.appendChild(st);
        if (waiting) {
          const need = document.createElement("span");
          need.className = "needs-input";
          need.textContent = "⚠ needs your input";
          row2.appendChild(need);
        }
        card.appendChild(row2);

        const meta = document.createElement("div");
        meta.className = "meta";
        const bits = [];
        if (hb.model) bits.push(hb.model);
        if (hb.branch) bits.push("⑂ " + hb.branch);
        if (hb.pr_url) bits.push("PR #" + (hb.pr_number || ""));
        if (hb.mcp_servers && hb.mcp_servers.length) bits.push("🔌 " + hb.mcp_servers.length);
        if (hb.device_pinned) bits.push("📌");
        if (hb.cwd) bits.push(hb.cwd);
        meta.textContent = bits.join("  ·  ");
        card.appendChild(meta);

        // Remark: a user-authored note labelling this session's task/goal, so
        // the operator can locate it at a glance. Persisted in localStorage
        // keyed by session id and survives the 2s re-render. Click the text (or
        // the ✎ when empty) to edit inline; stopPropagation keeps the card's
        // attach() from firing while editing.
        const remarkRow = document.createElement("div");
        remarkRow.className = "remark";
        const remarkText = document.createElement("span");
        remarkText.className = "remark-text";
        const curRemark = getRemark(s.id);
        if (curRemark) {
          remarkText.textContent = "📝 " + curRemark;
          remarkText.title = curRemark + "  (click to edit)";
        } else {
          remarkRow.classList.add("empty");
          remarkText.textContent = "✎ add remark";
          remarkText.title = "Add a note to identify this session's task";
        }
        remarkText.addEventListener("click", (e) => {
          e.stopPropagation();
          startRemarkEdit(remarkRow, s.id, curRemark);
        });
        remarkRow.appendChild(remarkText);
        card.appendChild(remarkRow);

        const actions = document.createElement("div");
        actions.className = "card-actions";

        // Clone: open a fresh, clean-context session in the same repo (cwd).
        // The server derives the cwd from this session's heartbeat, so the
        // browser only sends the source id.
        const cloneBtn = document.createElement("button");
        cloneBtn.className = "btn ghost small";
        cloneBtn.textContent = "⧉ New session";
        cloneBtn.title = "Open a fresh session in the same repo (clean context)";
        cloneBtn.addEventListener("click", (e) => {
          e.stopPropagation();
          cloneSession(s.id, hb, g.device);
        });
        actions.appendChild(cloneBtn);

        // Close: gracefully terminate this session (SIGTERM to its Claude
        // child, killing the terminal). Destructive/irreversible -> confirm.
        const closeBtn = document.createElement("button");
        closeBtn.className = "btn ghost small danger";
        closeBtn.textContent = "✕ Close";
        closeBtn.title = "Terminate this session and kill its terminal";
        closeBtn.addEventListener("click", (e) => {
          e.stopPropagation();
          killSession(s.id, hb);
        });
        actions.appendChild(closeBtn);
        card.appendChild(actions);

        card.addEventListener("click", () => attach(s.id, hb));
        wrap.appendChild(card);
      }
      list.appendChild(wrap);
    }
    processAttention(groups);
    // Keep the phone view's tap list in sync with the same 2s poll snapshot.
    if (mobileMode) renderMobileList(groups);
  }

  // ---- Mobile view rendering ------------------------------------------------

  // renderMobileList paints the phone tap list from the latest poll snapshot.
  // It reuses the desktop's visible-managed filter and stored ordering, but
  // sorts WAITING sessions first so a Claude permission prompt is one tap away.
  // Each card shows the workspace label, a state pill, and a "⚠ Needs input"
  // badge when waiting. While the action panel is open it refreshes that
  // session's state pill in place, and if the active session vanished (ended)
  // it closes the panel with a notice rather than leaving a dead terminal.
  function renderMobileList(groups) {
    const list = el("mobile-list");
    if (!list) return;

    // Flatten to the same set of cards the desktop shows (managed + attachable),
    // tagged with their device for the sub-label.
    const items = [];
    for (const g of groups || []) {
      const visible = g.sessions.filter(
        (s) => !(s.kind === "observed" || s.attachable === false)
      );
      for (const s of sortSessionsByOrder(visible)) items.push({ s, device: g.device });
    }
    // Waiting-first, then the user's stored order (already applied above, so a
    // stable partition preserves it within each bucket).
    items.sort((a, b) => (b.s.state === "waiting") - (a.s.state === "waiting"));

    list.innerHTML = "";
    if (items.length === 0) {
      const p = document.createElement("p");
      p.className = "muted";
      p.style.padding = "16px";
      p.textContent = "No interactive sessions yet.";
      list.appendChild(p);
    }
    for (const { s, device } of items) {
      const hb = s.heartbeat || {};
      const waiting = s.state === "waiting";
      const card = document.createElement("button");
      card.type = "button";
      card.className = "mobile-card" + (waiting ? " attention" : "");
      card.dataset.id = s.id;

      const label = document.createElement("div");
      label.className = "mobile-card-label";
      label.textContent = workspaceName(hb) || hb.command || s.id;
      card.appendChild(label);

      const sub = document.createElement("div");
      sub.className = "mobile-card-sub";
      const st = document.createElement("span");
      st.className = stateClass(s.state);
      st.textContent = s.state || "idle";
      sub.appendChild(st);
      const dev = document.createElement("span");
      dev.className = "muted";
      dev.textContent = device;
      sub.appendChild(dev);
      if (waiting) {
        const badge = document.createElement("span");
        badge.className = "mobile-badge";
        badge.textContent = "⚠ Needs input";
        sub.appendChild(badge);
      }
      card.appendChild(sub);

      card.addEventListener("click", () => openMobileAction(s.id, hb));
      list.appendChild(card);
    }

    // If the action panel is open, keep its state pill fresh; if that session
    // disappeared, close the panel with a notice.
    if (mobileActiveID !== null) {
      const active = items.find((it) => it.s.id === mobileActiveID);
      if (!active) {
        closeMobileAction("[session ended]");
      } else {
        const pill = el("mobile-state");
        pill.className = stateClass(active.s.state);
        pill.textContent = active.s.state || "idle";
      }
    }
  }

  // openMobileAction reveals the action panel for a session and opens its
  // read-only context stream. A lighter analogue of the desktop attach(): no
  // FitAddon, no resize (which would resize the shared PTY), no editable term.
  function openMobileAction(id, hb) {
    mobileActiveID = id;
    el("mobile-list").hidden = true;
    el("mobile-action").hidden = false;
    el("mobile-title").textContent = (hb && (workspaceName(hb) || hb.command)) || id;
    const pill = el("mobile-state");
    pill.className = "state idle";
    pill.textContent = "…";
    openMobileContext(id);
  }

  // ensureMobileTerm lazily creates the read-only context terminal. Mirrors
  // ensureTerm() but with stdin disabled and NO FitAddon / onData — the phone
  // never edits this terminal, it only reads it and sends discrete keystrokes
  // through the action buttons.
  function ensureMobileTerm() {
    if (mobileTerm) return;
    mobileTerm = new window.Terminal({
      disableStdin: true,
      cursorBlink: false,
      fontFamily: "Menlo, Monaco, 'Courier New', monospace",
      fontSize: 12,
      theme: { background: "#000000" },
    });
    mobileTerm.open(el("mobile-term"));
  }

  // openMobileContext opens the reused per-session terminal WebSocket into the
  // read-only mobileTerm. It mirrors the WS-open half of attach() but writes to
  // mobileSocket/mobileTerm and deliberately DOES NOT send a resize frame — the
  // PTY size is negotiated as the minimum across subscribers, so a phone resize
  // would shrink a desktop viewer's terminal.
  function openMobileContext(id) {
    ensureMobileTerm();
    mobileTerm.reset();
    if (mobileSocket) {
      try { mobileSocket.close(); } catch { /* ignore */ }
      mobileSocket = null;
    }
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/v1/terminal/${encodeURIComponent(id)}`;
    mobileSocket = new WebSocket(url);
    mobileSocket.binaryType = "arraybuffer";
    mobileSocket.onmessage = (ev) => {
      const data = ev.data instanceof ArrayBuffer
        ? decoder.decode(new Uint8Array(ev.data))
        : ev.data;
      mobileTerm.write(data, () => mobileTerm.scrollToBottom());
    };
    mobileSocket.onclose = () => {
      if (mobileActiveID === id) {
        mobileTerm.write("\r\n\x1b[90m[disconnected]\x1b[0m\r\n");
      }
    };
  }

  // sendMobileInput sends a keystroke byte sequence over the reused terminal
  // socket in the SAME shape the desktop uses (terminal.go decodes {type:
  // "input"} into sess.SendInput). No-op if the socket is not open yet.
  function sendMobileInput(bytes) {
    if (mobileSocket && mobileSocket.readyState === WebSocket.OPEN) {
      mobileSocket.send(JSON.stringify({ type: "input", data: bytes }));
    }
  }

  // closeMobileAction returns to the tap list, closing the context socket but
  // keeping mobileTerm allocated for reuse on the next open. An optional notice
  // is written into the terminal first (e.g. "[session ended]").
  function closeMobileAction(notice) {
    if (notice && mobileTerm) {
      mobileTerm.write("\r\n\x1b[90m" + notice + "\x1b[0m\r\n");
    }
    if (mobileSocket) {
      try { mobileSocket.close(); } catch { /* ignore */ }
      mobileSocket = null;
    }
    mobileActiveID = null;
    el("mobile-action").hidden = true;
    el("mobile-list").hidden = false;
  }

  // wireCardDrag attaches HTML5 drag-and-drop handlers to a session card so the
  // user can reorder cards within their device group by dragging the handle.
  // `visible` is the group's current display order and `vi` this card's index.
  //
  // Drop position is decided by the cursor's Y within the card under the
  // pointer: above the midpoint inserts BEFORE that card, below inserts AFTER.
  // A `.drop-before` / `.drop-after` class draws an insertion line so the target
  // slot is unambiguous. draggingID gates the poll re-render for the drag's
  // duration (cleared on dragend), so the card is never rebuilt mid-drag.
  function wireCardDrag(card, visible, vi) {
    card.addEventListener("dragstart", (e) => {
      draggingID = card.dataset.id;
      card.classList.add("dragging");
      e.dataTransfer.effectAllowed = "move";
      // Firefox requires data to be set for the drag to start.
      try { e.dataTransfer.setData("text/plain", card.dataset.id); } catch { /* ignore */ }
    });

    card.addEventListener("dragover", (e) => {
      if (draggingID === null || card.dataset.id === draggingID) return;
      e.preventDefault(); // allow drop
      e.dataTransfer.dropEffect = "move";
      const rect = card.getBoundingClientRect();
      const after = e.clientY > rect.top + rect.height / 2;
      card.classList.toggle("drop-after", after);
      card.classList.toggle("drop-before", !after);
    });

    const clearMarks = () => {
      card.classList.remove("drop-before", "drop-after");
    };
    card.addEventListener("dragleave", clearMarks);

    card.addEventListener("drop", (e) => {
      if (draggingID === null || card.dataset.id === draggingID) { clearMarks(); return; }
      e.preventDefault();
      e.stopPropagation();
      const rect = card.getBoundingClientRect();
      const after = e.clientY > rect.top + rect.height / 2;
      // Target slot in the group's order: this card's index, +1 when dropping
      // below its midpoint. The dragged card's source index is looked up by id
      // (draggingID) in the group order — the drop fires on the TARGET card, so
      // we can't read it from this card's index. reorderByDrop normalises the
      // source-after-target shift.
      const to = vi + (after ? 1 : 0);
      const from = visible.findIndex((s) => s.id === draggingID);
      clearMarks();
      if (from !== -1) reorderByDrop(visible, from, to);
    });

    card.addEventListener("dragend", () => {
      // Always clear drag state so a cancelled drag (Esc / drop outside) can't
      // wedge the poll re-render off. Clear stray marks on every card too.
      draggingID = null;
      card.classList.remove("dragging");
      const listEl = el("device-list");
      if (listEl) listEl.querySelectorAll(".drop-before, .drop-after")
        .forEach((c) => c.classList.remove("drop-before", "drop-after"));
    });
  }

  // startRemarkEdit swaps a card's remark row for an inline text input so the
  // user can label the session with its task/goal. Enter or blur saves to
  // localStorage (keyed by session id); Escape cancels. After saving we re-run
  // the list render so the new note shows immediately and the next 2s poll
  // won't clobber the edit-in-progress (edits complete synchronously here).
  function startRemarkEdit(remarkRow, sessionID, current) {
    remarkRow.innerHTML = "";
    remarkRow.classList.remove("empty");
    // Mark this session as being edited so the 2s poll's renderDevices() skips
    // its full re-render and does not blow away this input (which would fire
    // blur and close the edit prematurely — the reported bug).
    editingRemarkID = sessionID;
    const input = document.createElement("input");
    input.type = "text";
    input.className = "remark-input";
    input.value = current || "";
    input.placeholder = "Describe this session's task…";
    input.maxLength = 200;
    let done = false;
    const commit = (save) => {
      if (done) return;
      done = true;
      if (save) setRemark(sessionID, input.value);
      // Editing is finished: clear the guard first, then re-render so the poll
      // resumes updating this card normally on its next tick.
      editingRemarkID = null;
      poll(); // re-render the list from the latest snapshot
    };
    input.addEventListener("click", (e) => e.stopPropagation());
    input.addEventListener("keydown", (e) => {
      e.stopPropagation();
      if (e.key === "Enter") { e.preventDefault(); commit(true); }
      else if (e.key === "Escape") { e.preventDefault(); commit(false); }
    });
    // Blur now fires only on a genuine user focus change (clicking outside the
    // remark area) because renderDevices() no longer steals focus while this
    // edit is active. That is exactly the requested "finish by clicking a
    // non-add-remark part" behavior, and it saves what the user typed.
    input.addEventListener("blur", () => commit(true));
    remarkRow.appendChild(input);
    input.focus();
    input.select();
  }



  // processAttention scans the rendered snapshot for sessions in the "waiting"
  // state (Claude blocked on the user), then surfaces them three ways per the
  // chosen UX: a sidebar banner, a title badge, and a one-shot browser
  // Notification per newly-waiting session. Called on every poll so the banner
  // and title track the live set, but browser Notifications fire only on the
  // transition into waiting (tracked via alertedIDs) so a stuck session does
  // not re-alert every 2s.
  function processAttention(groups) {
    const waiting = [];
    for (const g of groups || []) {
      for (const s of g.sessions || []) {
        // Observed sessions are hidden from the dashboard, so they must not
        // raise attention banners/toasts either.
        if (s.kind === "observed" || s.attachable === false) continue;
        if (s.state === "waiting") {
          waiting.push({
            id: s.id,
            device: g.device,
            hb: s.heartbeat || {},
            // Retain enough of the session to route the toast's Open button:
            // managed sessions attach a terminal, observed ones open the panel.
            attachable: !!s.attachable,
            session: s,
          });
        }
      }
    }

    // Sidebar banner + title badge reflect the current count.
    const banner = el("attention-banner");
    if (waiting.length > 0) {
      const n = waiting.length;
      banner.textContent = "⚠ " + n + " session" + (n === 1 ? "" : "s") + " waiting for you";
      banner.hidden = false;
      document.title = "(" + n + ") " + baseTitle;
    } else {
      banner.hidden = true;
      document.title = baseTitle;
    }

    // Fire a browser Notification AND an in-page toast once per transition into
    // waiting. The toast is the primary in-UI prompt (works even when OS
    // Notifications are denied); notify() is a best-effort OS-level extra.
    const nowWaiting = new Set(waiting.map((w) => w.id));
    for (const w of waiting) {
      if (!alertedIDs.has(w.id)) {
        alertedIDs.add(w.id);
        notify(w);
        showToast(w);
      }
    }
    // Drop ids that are no longer waiting so a later re-entry alerts again, and
    // dismiss any lingering toast for a session that resolved on its own.
    for (const id of Array.from(alertedIDs)) {
      if (!nowWaiting.has(id)) {
        alertedIDs.delete(id);
        dismissToast(id);
      }
    }
  }

  // ---- In-page attention toasts --------------------------------------------

  // showToast pops a small dismissable card into the bottom-right stack naming a
  // newly-waiting session, with an Open button that jumps to it. It is keyed by
  // session id so a session only ever has one toast; a re-entry replaces it.
  function showToast(w) {
    const stack = el("toast-stack");
    if (!stack) return;
    dismissToast(w.id); // replace any existing toast for this id

    const label = workspaceName(w.hb) || (w.hb && w.hb.command) || w.id;

    const toast = document.createElement("div");
    toast.className = "toast";
    toast.dataset.id = w.id;

    const head = document.createElement("div");
    head.className = "toast-head";
    const icon = document.createElement("span");
    icon.className = "toast-icon";
    icon.textContent = "⚠";
    const title = document.createElement("span");
    title.className = "toast-title";
    title.textContent = "Needs your input";
    const close = document.createElement("button");
    close.className = "toast-close";
    close.type = "button";
    close.title = "Dismiss";
    close.textContent = "×";
    close.addEventListener("click", (e) => { e.stopPropagation(); dismissToast(w.id); });
    head.appendChild(icon);
    head.appendChild(title);
    head.appendChild(close);
    toast.appendChild(head);

    const body = document.createElement("div");
    body.className = "toast-body";
    body.textContent = label + " · " + w.device +
      (w.hb && w.hb.branch ? " (⑂ " + w.hb.branch + ")" : "");
    toast.appendChild(body);

    const actions = document.createElement("div");
    actions.className = "toast-actions";
    const open = document.createElement("button");
    open.className = "btn primary small";
    open.type = "button";
    open.textContent = "Open";
    open.addEventListener("click", (e) => {
      e.stopPropagation();
      openWaiting(w);
      dismissToast(w.id);
    });
    actions.appendChild(open);
    toast.appendChild(actions);

    // Clicking the toast body also opens the session.
    toast.addEventListener("click", () => { openWaiting(w); dismissToast(w.id); });

    stack.appendChild(toast);
  }

  // openWaiting jumps to a waiting session: managed sessions attach a terminal,
  // observed processes open their read-only detail panel. In phone mode a
  // managed session opens the mobile action panel instead of the desktop
  // terminal so the toast's Open button stays consistent with the active view.
  function openWaiting(w) {
    if (w.attachable) {
      if (mobileMode) openMobileAction(w.id, w.hb);
      else attach(w.id, w.hb);
    } else if (w.session) {
      showObserved(w.session);
    }
  }

  // dismissToast removes the toast for a given session id, if present.
  function dismissToast(id) {
    const stack = el("toast-stack");
    if (!stack) return;
    const t = stack.querySelector('.toast[data-id="' + cssEscape(id) + '"]');
    if (t) t.remove();
  }

  // cssEscape is a tiny attribute-selector escaper for session ids that may
  // contain characters (: / .) unsafe in a querySelector attribute value.
  function cssEscape(s) {
    if (window.CSS && CSS.escape) return CSS.escape(String(s));
    return String(s).replace(/["\\]/g, "\\$&");
  }

  // notify raises a browser Notification for a newly-waiting session, lazily
  // requesting permission the first time. If permission is denied the banner and
  // title badge still convey the state.
  function notify(w) {
    if (typeof Notification === "undefined") return;
    const title = "Claude needs you";
    const body = ((w.hb.command || "session") + " on " + w.device +
      (w.hb.cwd ? " · " + w.hb.cwd : "")).trim();

    if (notifyPermission === "granted") {
      try { new Notification(title, { body, tag: "agent-beacon-" + w.id }); } catch { /* ignore */ }
    } else if (notifyPermission === "default") {
      Notification.requestPermission().then((p) => {
        notifyPermission = p;
        if (p === "granted") {
          try { new Notification(title, { body, tag: "agent-beacon-" + w.id }); } catch { /* ignore */ }
        }
      });
    }
  }

  async function poll() {
    const res = await sessionsProbe();
    if (!res.ok) {
      if (res.status === 401) {
        stopPolling();
        showLogin(await loginInfo());
      }
      return;
    }
    renderDevices(res.data);
    renderOrchList();
  }

  function startPolling() {
    stopPolling();
    poll();
    pollTimer = setInterval(poll, 2000);
  }
  function stopPolling() {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  }

  // ---- Terminal attach ------------------------------------------------------

  function ensureTerm() {
    if (term) return;
    term = new window.Terminal({
      cursorBlink: true,
      fontFamily: "Menlo, Monaco, 'Courier New', monospace",
      fontSize: 13,
      theme: { background: "#000000" },
    });
    fitAddon = new window.FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(el("terminal"));
    // Fit once the font is measurable. FitAddon derives cols from the measured
    // monospace cell width, so a fit before the font loads yields a wrong width
    // and mis-wrapped output. When fonts are already loaded this resolves on the
    // next microtask; otherwise we defer the re-fit until the font is ready.
    if (document.fonts && document.fonts.ready) {
      document.fonts.ready.then(() => scheduleFit());
    }
    safeFit();

    term.onData((data) => {
      if (termSocket && termSocket.readyState === WebSocket.OPEN) {
        termSocket.send(JSON.stringify({ type: "input", data }));
      }
    });

    // Track whether the viewport is at the bottom. Once the user scrolls up we
    // stop auto-pinning (so they can read history); when they return to the
    // bottom we resume pinning. Registered once here since `term` is reused.
    term.onScroll(() => {
      const buf = term.buffer.active;
      stickToBottom = buf.viewportY >= buf.length - term.rows - 1;
    });

    // Re-fit on ANY host size change, debounced. This is the primary fix for
    // intermittent wrapping: container changes that don't fire window.resize
    // (sidebar/pill re-layout, panel show/hide) now still re-fit the terminal.
    if (window.ResizeObserver) {
      termResizeObserver = new ResizeObserver(() => scheduleFit());
      termResizeObserver.observe(el("terminal"));
    }
    // Keep window.resize too: it covers device pixel-ratio / zoom changes that
    // do not alter the host's CSS box but do change the measured cell size.
    window.addEventListener("resize", () => scheduleFit());
  }

  // safeFit fits only when a fit can produce a correct width (font measured and
  // the host is big enough to hold at least one cell). Guards against fitting a
  // hidden panel (display:none → 0 box) or a padding-only collapsed box, either
  // of which would compute a degenerate 1-column size. The 24px floor clears the
  // host's 2×8px padding so we never fit against a content box narrower than a
  // single character cell.
  function safeFit() {
    if (!fitAddon || !term) return;
    const host = el("terminal");
    if (!host || host.clientWidth < 24 || host.clientHeight < 24) return;
    try { fitAddon.fit(); } catch { /* xterm not ready yet */ }
  }

  // scheduleFit coalesces rapid resize signals into a single fit + resize frame
  // on a short timer, so a window drag or a burst of ResizeObserver callbacks
  // settles on the final size instead of thrashing through intermediates.
  function scheduleFit() {
    if (fitDebounceTimer) clearTimeout(fitDebounceTimer);
    fitDebounceTimer = setTimeout(() => {
      fitDebounceTimer = null;
      safeFit();
      sendResize();
    }, 80);
  }

  function sendResize() {
    if (term && termSocket && termSocket.readyState === WebSocket.OPEN) {
      termSocket.send(JSON.stringify({ type: "resize", rows: term.rows, cols: term.cols }));
    }
  }

  function attach(id, hb) {
    if (id === activeSessionID) return;
    detach();
    activeSessionID = id;

    closeOrchSocket();
    orchActiveRunID = null;
    hideAllStagePanels();
    el("terminal-panel").hidden = false;
    el("term-session").textContent = (hb && hb.command) || id;
    el("term-meta").textContent = id;

    ensureTerm();
    term.reset();

    // Defer opening the WebSocket until AFTER the freshly-revealed terminal
    // panel has been laid out and fitted to its FINAL size. The server sends the
    // session scrollback backlog as the first frame, and term.write() parses
    // asynchronously; if we fit after that backlog is written, xterm reflows the
    // already-written rows to the new column count, splitting/joining lines and
    // drifting the cursor. Fitting first (double-rAF, so the panel is laid out
    // before we measure) makes the first backlog frame land at the correct
    // width — no reflow race.
    requestAnimationFrame(() => requestAnimationFrame(() => {
      // A later attach()/detach() may have won during the two rAF ticks; bail if
      // this attach is no longer the active one.
      if (activeSessionID !== id) return;

      safeFit();

      const proto = location.protocol === "https:" ? "wss:" : "ws:";
      const url = `${proto}//${location.host}/api/v1/terminal/${encodeURIComponent(id)}`;
      termSocket = new WebSocket(url);
      termSocket.binaryType = "arraybuffer";

      // On (re)attach we want the viewport pinned to the newest output, so a
      // long backlog doesn't leave the user scrolled up in history. The scroll
      // listener that maintains this flag is registered once in ensureTerm().
      stickToBottom = true;

      // The fit already happened before the socket opened, so we only report the
      // settled size — no second re-fit that could reflow the backlog.
      termSocket.onopen = () => { sendResize(); term.focus(); };
      termSocket.onmessage = (ev) => {
        const data = ev.data instanceof ArrayBuffer
          ? decoder.decode(new Uint8Array(ev.data))
          : ev.data;
        // write() buffers/parses asynchronously; scroll in its callback so the
        // new rows already exist in the buffer when we pin to the bottom.
        term.write(data, () => { if (stickToBottom) term.scrollToBottom(); });
      };
      termSocket.onclose = () => {
        if (activeSessionID === id) term.write("\r\n\x1b[90m[disconnected]\x1b[0m\r\n");
      };
    }));

    // Re-render list to highlight the active card.
    poll();
  }

  // hideAllStagePanels is the single source of truth for stage visibility.
  // Every "show" entry point calls this first, then reveals only its own panel,
  // guaranteeing exactly one stage panel is ever visible.
  function hideAllStagePanels() {
    el("empty-stage").hidden = true;
    el("observed-panel").hidden = true;
    el("terminal-panel").hidden = true;
    el("orch-launch-panel").hidden = true;
    el("orch-run-panel").hidden = true;
  }

  function detach() {
    if (termSocket) {
      try { termSocket.close(); } catch { /* ignore */ }
      termSocket = null;
    }
    activeSessionID = null;
    hideAllStagePanels();
    el("empty-stage").hidden = false;
  }

  // ---- Observed (read-only) inspection --------------------------------------

  // showObserved renders a discovered process's metadata in the read-only panel.
  // Observed processes have no PTY, so there is deliberately no terminal here.
  function showObserved(s) {
    detach();
    activeSessionID = s.id;
    const hb = s.heartbeat || {};

    closeOrchSocket();
    orchActiveRunID = null;
    hideAllStagePanels();
    el("observed-panel").hidden = false;
    el("obs-session").textContent = hb.command || s.id;
    el("obs-meta").textContent = s.id;

    const project = hb.cwd ? hb.cwd.replace(/\/+$/, "").split("/").pop() : "";
    const fields = [
      ["State", s.state || "idle"],
      ["PID", s.pid ? String(s.pid) : ""],
      ["Project", project],
      ["Model", hb.model || ""],
      ["Branch", hb.branch || ""],
      ["Directory", hb.cwd || ""],
      ["Task", hb.task || ""],
      ["Context", hb.context_pct ? hb.context_pct + "%" : ""],
      ["Source", hb.session_source || ""],
    ];
    const dl = el("obs-fields");
    dl.innerHTML = "";
    for (const [k, v] of fields) {
      if (!v) continue;
      const dt = document.createElement("dt");
      dt.textContent = k;
      const dd = document.createElement("dd");
      dd.textContent = v;
      dl.appendChild(dt);
      dl.appendChild(dd);
    }
    // Pull Request needs a clickable anchor, not plain text.
    if (hb.pr_url) {
      const dt = document.createElement("dt");
      dt.textContent = "Pull Request";
      const dd = document.createElement("dd");
      const a = document.createElement("a");
      a.href = hb.pr_url;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      a.textContent =
        "#" + (hb.pr_number || "") + (hb.pr_state ? " (" + hb.pr_state + ")" : "");
      dd.appendChild(a);
      dl.appendChild(dt);
      dl.appendChild(dd);
    }
    // MCP servers render as small chips.
    if (hb.mcp_servers && hb.mcp_servers.length) {
      const dt = document.createElement("dt");
      dt.textContent = "MCP servers";
      const dd = document.createElement("dd");
      for (const name of hb.mcp_servers) {
        const chip = document.createElement("span");
        chip.className = "mcp-chip";
        chip.textContent = name;
        dd.appendChild(chip);
      }
      dl.appendChild(dt);
      dl.appendChild(dd);
    }
    // Highlight the selected card.
    poll();
  }

  // ---- Spawn ----------------------------------------------------------------

  let spawnDevice = null;

  function openSpawn(device, projects) {
    spawnDevice = device;
    el("spawn-device").textContent = device;
    el("spawn-error").hidden = true;
    el("spawn-command").value = "";
    el("spawn-branch").value = "";
    el("spawn-path").value = "";
    const sel = el("spawn-project");
    sel.innerHTML = "";
    for (const p of projects) {
      const opt = document.createElement("option");
      opt.value = p.path;
      opt.textContent = p.name;
      sel.appendChild(opt);
    }
    el("spawn-modal").hidden = false;
  }

  function closeSpawn() {
    el("spawn-modal").hidden = true;
    spawnDevice = null;
  }

  async function doSpawn() {
    const errBox = el("spawn-error");
    errBox.hidden = true;
    // A manual absolute path (untrusted) overrides the project dropdown; the
    // agent still validates it against its allowed roots.
    const manual = el("spawn-path").value.trim();
    const projectPath = manual || el("spawn-project").value;
    const device = spawnDevice;
    const body = {
      device: device,
      project_path: projectPath,
      command: el("spawn-command").value.trim(),
      worktree_branch: el("spawn-branch").value.trim(),
      worktree_location: el("spawn-location").value,
    };
    // Snapshot existing managed session ids on this device so we can detect the
    // agent-generated id of the new session once it connects.
    const before = idsForDevice(await fetchGroups(), device);
    try {
      const r = await fetch("/api/v1/spawn", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "same-origin",
        body: JSON.stringify(body),
      });
      if (r.status === 202) {
        closeSpawn();
        autoAttachNew(device, projectPath, before);
        return;
      }
      const j = await r.json().catch(() => ({}));
      errBox.textContent = j.error || ("Spawn failed (" + r.status + ").");
      errBox.hidden = false;
    } catch {
      errBox.textContent = "Spawn request failed. Is the server reachable?";
      errBox.hidden = false;
    }
  }

  // fetchGroups returns the current device groups (or [] on failure), wrapping
  // sessionsProbe so callers can snapshot/diff sessions without touching auth.
  async function fetchGroups() {
    const res = await sessionsProbe();
    return res.ok ? (res.data || []) : [];
  }

  // idsForDevice returns a Set of managed session ids on the given device.
  function idsForDevice(groups, device) {
    const ids = new Set();
    for (const g of groups || []) {
      if (g.device !== device) continue;
      for (const s of g.sessions || []) {
        const observed = s.kind === "observed" || s.attachable === false;
        if (!observed) ids.add(s.id);
      }
    }
    return ids;
  }

  // cloneSession opens a fresh session in the same cwd as sourceId. The server
  // derives the cwd (server-authoritative), so we send only the source id; on
  // success we auto-attach the new session's live terminal.
  async function cloneSession(sourceId, hb, device) {
    const before = idsForDevice(await fetchGroups(), device);
    try {
      const r = await fetch(
        "/api/v1/sessions/" + encodeURIComponent(sourceId) + "/clone",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          credentials: "same-origin",
          body: "{}",
        },
      );
      const j = await r.json().catch(() => ({}));
      if (r.status === 202) {
        autoAttachNew(j.device || device, j.cwd || (hb && hb.cwd), before);
        return;
      }
      alert(j.error || ("Clone failed (" + r.status + ")."));
    } catch {
      alert("Clone request failed. Is the server reachable?");
    }
  }

  // killSession asks the server to gracefully terminate a managed session
  // (SIGTERM to its Claude child, which ends the PTY/wrapper = the terminal is
  // killed). Irreversible, so it is confirm-gated. If the session being killed
  // is the one currently attached, detach the terminal panel first. A re-poll
  // refreshes the card list once the session drops.
  async function killSession(id, hb) {
    const name = (hb && hb.command) || id;
    if (!confirm('Close session "' + name + '"?\n\nThis terminates the process and kills its terminal. This cannot be undone.')) {
      return;
    }
    try {
      const r = await fetch(
        "/api/v1/sessions/" + encodeURIComponent(id),
        {
          method: "DELETE",
          credentials: "same-origin",
        },
      );
      const j = await r.json().catch(() => ({}));
      if (r.status === 202) {
        if (id === activeSessionID) detach();
        poll();
        return;
      }
      alert(j.error || ("Close failed (" + r.status + ")."));
    } catch {
      alert("Close request failed. Is the server reachable?");
    }
  }

  // autoAttachNew polls for a newly-connected managed session on device (one not
  // present in `before`), preferring an exact cwd match, then attaches its live
  // terminal so the user immediately sees a real session. The new id is
  // agent-generated, so diff-detection against `before` is required.
  function autoAttachNew(device, cwd, before) {
    const deadline = Date.now() + 8000;
    const tick = async () => {
      const groups = await fetchGroups();
      renderDevices(groups);
      let anyNew = null;
      let cwdMatch = null;
      for (const g of groups) {
        if (g.device !== device) continue;
        for (const s of g.sessions || []) {
          const observed = s.kind === "observed" || s.attachable === false;
          if (observed || before.has(s.id)) continue;
          anyNew = anyNew || s;
          const shb = s.heartbeat || {};
          if (cwd && shb.cwd === cwd) cwdMatch = cwdMatch || s;
        }
      }
      const target = cwdMatch || anyNew;
      if (target) {
        attach(target.id, target.heartbeat || {});
        return;
      }
      if (Date.now() < deadline) setTimeout(tick, 500);
    };
    setTimeout(tick, 500);
  }

  // ---- Orchestration (UI-driven multi-agent loop) ---------------------------

  // The orchestrate flow: user submits a task + repo, the server runs the
  // plan -> parallel worktree workers -> Gate 1 (build/test) -> Gate 2
  // (cross-model verifier) -> self-repair loop, and streams progress events
  // over a WebSocket we render as a per-subtask progress tree + live log.

  let orchSocket = null;
  let orchActiveRunID = null;
  let orchSubtasks = null; // Map<subtaskID, {card, iterEl, gateEl, verdictEl}>
  let orchPendingReqID = null; // the open intervention req_id we can answer, if any

  async function orchListProbe() {
    const r = await fetch("/api/v1/orchestrations", { credentials: "same-origin" });
    if (!r.ok) return [];
    return await r.json().catch(() => []);
  }

  function statusPillClass(status, passed) {
    if (status === "running") return "pill running";
    if (status === "waiting") return "pill waiting";
    if (status === "error") return "pill offline";
    if (status === "done") return passed ? "pill online" : "pill offline";
    return "pill";
  }

  // fmtDuration renders a millisecond duration as a compact human string.
  function fmtDuration(ms) {
    if (!ms || ms <= 0) return "";
    if (ms < 1000) return ms + "ms";
    if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
    return Math.floor(ms / 60000) + "m " + Math.round((ms % 60000) / 1000) + "s";
  }

  async function renderOrchList() {
    const runs = await orchListProbe();
    const list = el("orch-list");
    list.innerHTML = "";
    if (!runs || runs.length === 0) {
      const p = document.createElement("p");
      p.className = "muted";
      p.style.padding = "8px 10px";
      p.textContent = "No runs yet. Click + to start one.";
      list.appendChild(p);
      return;
    }
    for (const run of runs) {
      const card = document.createElement("div");
      card.className = "orch-card" + (run.id === orchActiveRunID ? " active" : "");
      card.dataset.id = run.id;

      const row1 = document.createElement("div");
      row1.className = "row1";
      const task = document.createElement("span");
      task.className = "cmd";
      task.textContent = run.task;
      row1.appendChild(task);
      const st = document.createElement("span");
      st.className = statusPillClass(run.status, run.passed);
      st.textContent = run.status === "done" ? (run.passed ? "pass" : "fail") : run.status;
      row1.appendChild(st);

      // Delete button: removes a finished run from the store and disk. Hidden
      // for active runs (running/waiting) since the server refuses those (409).
      if (run.status === "done" || run.status === "error") {
        const del = document.createElement("button");
        del.className = "orch-del";
        del.title = "Delete run";
        del.textContent = "\u2715"; // ✕
        del.addEventListener("click", (ev) => {
          ev.stopPropagation();
          deleteRun(run.id);
        });
        row1.appendChild(del);
      }
      card.appendChild(row1);

      const meta = document.createElement("div");
      meta.className = "meta";
      meta.textContent = run.repo;
      card.appendChild(meta);

      card.addEventListener("click", () => openRun(run.id));
      list.appendChild(card);
    }
  }

  // deleteRun removes a finished run via DELETE and refreshes the list so the
  // card disappears immediately. If the deleted run is the one open in the
  // panel, reset back to the launcher view. A 409 (active run) or other error
  // is surfaced via a brief alert; the list is refreshed regardless.
  async function deleteRun(id) {
    if (!confirm("Delete this orchestration run? This cannot be undone.")) return;
    try {
      const r = await fetch("/api/v1/orchestrations/" + encodeURIComponent(id), {
        method: "DELETE",
        credentials: "same-origin",
      });
      if (!r.ok && r.status !== 204) {
        const d = await r.json().catch(() => ({}));
        alert(d.error || "Failed to delete run (" + r.status + ")");
      } else if (id === orchActiveRunID) {
        showOrchLaunch();
      }
    } catch (e) {
      alert("Failed to delete run");
    }
    renderOrchList();
  }

  // ---- Launcher -------------------------------------------------------------

  function showOrchLaunch() {
    detach();
    closeOrchSocket();
    orchActiveRunID = null;
    hideAllStagePanels();
    el("orch-launch-panel").hidden = false;
    el("orch-error").hidden = true;
    // Populate the allowed-roots datalist for convenience.
    populateOrchRoots();
    renderOrchList();
  }

  async function populateOrchRoots() {
    // The server does not expose roots directly; the projects announced by
    // monitors are a reasonable set of pickable repos, so offer those.
    try {
      const res = await sessionsProbe();
      if (!res.ok) return;
      const dl = el("orch-roots");
      dl.innerHTML = "";
      const seen = new Set();
      for (const g of res.data || []) {
        for (const p of g.projects || []) {
          if (seen.has(p.path)) continue;
          seen.add(p.path);
          const opt = document.createElement("option");
          opt.value = p.path;
          dl.appendChild(opt);
        }
      }
    } catch { /* ignore */ }
  }

  async function startOrchestration(ev) {
    ev.preventDefault();
    const errBox = el("orch-error");
    errBox.hidden = true;
    const task = el("orch-task").value.trim();
    const repo = el("orch-repo").value.trim();
    if (!task) {
      errBox.textContent = "Task is required.";
      errBox.hidden = false;
      return;
    }
    const body = { task, repo };
    const workers = parseInt(el("orch-workers").value, 10);
    const maxIters = parseInt(el("orch-maxiters").value, 10);
    if (!Number.isNaN(workers) && workers > 0) body.workers = workers;
    if (!Number.isNaN(maxIters) && maxIters > 0) body.max_iters = maxIters;
    const testCmd = el("orch-testcmd").value.trim();
    if (testCmd) body.test_cmd = testCmd;
    try {
      const r = await fetch("/api/v1/orchestrations", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "same-origin",
        body: JSON.stringify(body),
      });
      const j = await r.json().catch(() => ({}));
      if (r.status !== 202) {
        errBox.textContent = j.error || ("Start failed (" + r.status + ").");
        errBox.hidden = false;
        return;
      }
      el("orch-task").value = "";
      el("orch-testcmd").value = "";
      openRun(j.id);
      renderOrchList();
    } catch {
      errBox.textContent = "Start request failed. Is the server reachable?";
      errBox.hidden = false;
    }
  }

  // ---- Live run view --------------------------------------------------------

  function closeOrchSocket() {
    if (orchSocket) {
      try { orchSocket.close(); } catch { /* ignore */ }
      orchSocket = null;
    }
  }

  async function openRun(id) {
    detach();
    closeOrchSocket();
    orchActiveRunID = id;
    orchSubtasks = new Map();
    orchPendingReqID = null;
    closeInterventionModal();
    el("orch-guide-btn").hidden = true;

    hideAllStagePanels();
    el("orch-run-panel").hidden = false;

    el("orch-subtasks").innerHTML = "";
    el("orch-log").textContent = "";
    const report = el("orch-report");
    report.hidden = true;
    report.innerHTML = "";
    const diff = el("orch-diff");
    diff.hidden = true;
    diff.textContent = "";

    // Seed the header from the detail endpoint, then stream events.
    try {
      const r = await fetch("/api/v1/orchestrations/" + encodeURIComponent(id), { credentials: "same-origin" });
      if (r.ok) {
        const d = await r.json();
        renderRunHeader(d.run);
        if (d.report) renderReport(d.report);
      }
    } catch { /* ignore */ }

    renderOrchList();
    openOrchSocket(id);
  }

  function renderRunHeader(run) {
    if (!run) return;
    el("orch-run-title").textContent = run.task || run.id;
    let meta = run.repo || "";
    const dur = fmtDuration(run.duration_ms);
    if (dur) meta += (meta ? " · " : "") + dur;
    el("orch-run-meta").textContent = meta;
    const st = el("orch-run-status");
    st.className = statusPillClass(run.status, run.passed);
    st.textContent = run.status === "done" ? (run.passed ? "passed" : "failed") : run.status;
    // The Guide button can only answer an open intervention request, so it is
    // shown only while the run is waiting (there is a pending req_id).
    el("orch-guide-btn").hidden = !(run.status === "waiting" && orchPendingReqID);
  }

  function openOrchSocket(id) {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/v1/orchestrations/${encodeURIComponent(id)}/events`;
    orchSocket = new WebSocket(url);
    orchSocket.onmessage = (ev) => {
      let obj;
      try { obj = JSON.parse(ev.data); } catch { return; }
      handleOrchEvent(obj);
    };
    orchSocket.onclose = () => {
      // Run finished or socket dropped: refresh header/report from REST so the
      // final status is authoritative even if we missed the terminal event.
      if (orchActiveRunID === id) refreshRunDetail(id);
    };
  }

  async function refreshRunDetail(id) {
    try {
      const r = await fetch("/api/v1/orchestrations/" + encodeURIComponent(id), { credentials: "same-origin" });
      if (!r.ok) return;
      const d = await r.json();
      renderRunHeader(d.run);
      if (d.report) renderReport(d.report);
    } catch { /* ignore */ }
    renderOrchList();
  }

  // ensureSubtask returns (creating if needed) the progress card for a subtask.
  function ensureSubtask(id, branch) {
    const key = id || branch || "main";
    if (orchSubtasks.has(key)) return orchSubtasks.get(key);
    const card = document.createElement("div");
    card.className = "orch-subtask";

    const head = document.createElement("div");
    head.className = "orch-subtask-head";
    const title = document.createElement("strong");
    title.textContent = branch ? ("⑂ " + branch) : ("subtask " + key);
    head.appendChild(title);
    const statusEl = document.createElement("span");
    statusEl.className = "pill running";
    statusEl.textContent = "running";
    head.appendChild(statusEl);
    card.appendChild(head);

    const goalEl = document.createElement("div");
    goalEl.className = "orch-subtask-goal";
    card.appendChild(goalEl);

    const steps = document.createElement("div");
    steps.className = "orch-steps";
    const iterEl = document.createElement("div");
    iterEl.className = "orch-step";
    const gateEl = document.createElement("div");
    gateEl.className = "orch-step";
    const verdictEl = document.createElement("div");
    verdictEl.className = "orch-step";
    steps.appendChild(iterEl);
    steps.appendChild(gateEl);
    steps.appendChild(verdictEl);
    card.appendChild(steps);

    el("orch-subtasks").appendChild(card);
    const rec = { card, statusEl, goalEl, iterEl, gateEl, verdictEl };
    orchSubtasks.set(key, rec);
    return rec;
  }

  function appendLog(line) {
    const log = el("orch-log");
    log.textContent += line + "\n";
    log.scrollTop = log.scrollHeight;
  }

  function handleOrchEvent(ev) {
    const t = ev.time ? new Date(ev.time).toLocaleTimeString() : "";
    switch (ev.kind) {
      case "plan":
        appendLog(`[${t}] plan: ${ev.message || ""}`);
        break;
      case "worker-start": {
        const s = ensureSubtask(ev.subtask_id, ev.branch);
        s.statusEl.className = "pill running";
        s.statusEl.textContent = "running";
        if (s.goalEl) s.goalEl.textContent = ev.message || "";
        appendLog(`[${t}] worker start ${ev.branch || ev.subtask_id || ""}: ${ev.message || ""}`);
        break;
      }
      case "iteration": {
        const s = ensureSubtask(ev.subtask_id, ev.branch);
        s.iterEl.textContent = "iteration " + (ev.iteration || "?");
        appendLog(`[${t}] iteration ${ev.iteration || "?"} ${ev.branch || ""}`);
        break;
      }
      case "agent":
        appendLog(`[${t}] agent: ${ev.message || ""}`);
        break;
      case "gate": {
        const s = ensureSubtask(ev.subtask_id, ev.branch);
        const ok = ev.passed === true;
        s.gateEl.textContent = "Gate 1 (build/test): " + (ev.passed == null ? "…" : (ok ? "pass" : "fail"));
        s.gateEl.className = "orch-step " + (ev.passed == null ? "" : (ok ? "ok" : "err"));
        appendLog(`[${t}] gate ${ev.branch || ""}: ${ev.passed == null ? "running" : (ok ? "pass" : "fail")} ${ev.message || ""}`);
        break;
      }
      case "verdict": {
        const s = ensureSubtask(ev.subtask_id, ev.branch);
        const ok = ev.passed === true;
        s.verdictEl.textContent = "Gate 2 (verifier): " + (ev.passed == null ? "…" : (ok ? "pass" : "fail"));
        s.verdictEl.className = "orch-step " + (ev.passed == null ? "" : (ok ? "ok" : "err"));
        appendLog(`[${t}] verdict ${ev.branch || ""}: ${ev.passed == null ? "running" : (ok ? "pass" : "fail")} ${ev.message || ""}`);
        break;
      }
      case "self-repair": {
        // A recoverable env/infra failure the loop auto-fixed. Note it on the
        // subtask card (if any) and log it distinctly.
        const s = ensureSubtask(ev.subtask_id, ev.branch);
        const note = document.createElement("div");
        note.className = "orch-repair";
        note.textContent = "↻ self-repair: " + (ev.message || "");
        s.card.appendChild(note);
        appendLog(`[${t}] ↻ self-repair ${ev.branch || ""}: ${ev.message || ""}${ev.detail ? " — " + ev.detail : ""}`);
        break;
      }
      case "auth-needed": {
        // A dangerous op was observed; the run is paused awaiting authorization.
        setRunWaiting();
        appendLog(`[${t}] ⚠ authorization needed ${ev.branch || ""}: ${ev.message || ""}`);
        openInterventionModal({
          mode: "auth",
          reqId: ev.req_id,
          summary: ev.message || "A dangerous operation needs authorization.",
          detail: ev.detail || "",
        });
        break;
      }
      case "input-needed": {
        // The run paused for optional guidance (e.g. iteration budget exhausted).
        setRunWaiting();
        appendLog(`[${t}] ⏸ guidance requested ${ev.branch || ""}: ${ev.message || ""}`);
        openInterventionModal({
          mode: "input",
          reqId: ev.req_id,
          summary: ev.message || "The run paused for optional guidance.",
          detail: ev.detail || "",
        });
        break;
      }
      case "worker-done": {
        const s = ensureSubtask(ev.subtask_id, ev.branch);
        const ok = ev.passed === true;
        s.statusEl.className = ok ? "pill online" : "pill offline";
        s.statusEl.textContent = ok ? "pass" : "fail";
        appendLog(`[${t}] worker done ${ev.branch || ""}: ${ok ? "pass" : "fail"} ${ev.message || ""}`);
        break;
      }
      case "log":
        appendLog(`[${t}] ${ev.message || ""}`);
        break;
      case "run-done": {
        const st = el("orch-run-status");
        const ok = ev.passed === true;
        st.className = ok ? "pill online" : "pill offline";
        st.textContent = ok ? "passed" : "failed";
        appendLog(`[${t}] run complete: ${ok ? "PASSED" : "FAILED"} ${ev.message || ""}`);
        break;
      }
      default:
        if (ev.message) appendLog(`[${t}] ${ev.message}`);
    }
  }

  // setRunWaiting flips the run status pill to the amber "waiting" state used
  // while an intervention (auth or guidance) is pending.
  function setRunWaiting() {
    const st = el("orch-run-status");
    st.className = "pill waiting";
    st.textContent = "waiting";
  }

  // openInterventionModal shows the pause dialog. mode "auth" gates a dangerous
  // op (Approve & continue / Deny); mode "input" solicits optional guidance
  // (Send guidance / Skip). Either way the reply is delivered to the blocked run
  // goroutine via POST /respond, keyed by req_id.
  function openInterventionModal(opts) {
    orchPendingReqID = opts.reqId || null;
    const isAuth = opts.mode === "auth";
    el("orch-input-title").textContent = isAuth ? "Authorization required" : "Run paused";
    el("orch-input-summary").textContent = opts.summary || "";
    const detailEl = el("orch-input-detail");
    if (opts.detail) {
      detailEl.textContent = opts.detail;
      detailEl.hidden = false;
    } else {
      detailEl.textContent = "";
      detailEl.hidden = true;
    }
    el("orch-input-guidance").value = "";
    const errEl = el("orch-input-error");
    errEl.hidden = true;
    errEl.textContent = "";
    // Button labels differ by mode; "approve" carries approve=true either way.
    el("orch-input-approve").textContent = isAuth ? "Approve & continue" : "Send guidance";
    el("orch-input-deny").textContent = isAuth ? "Deny" : "Skip";
    // The proactive Guide button is only meaningful while a req is open.
    el("orch-guide-btn").hidden = !orchPendingReqID;
    el("orch-input-modal").hidden = false;
  }

  function closeInterventionModal() {
    el("orch-input-modal").hidden = true;
  }

  // submitIntervention delivers the user's decision to the paused run. On
  // success the modal closes and the run resumes streaming; a 409 means the
  // request already resolved (e.g. it timed out) so we close and refresh.
  async function submitIntervention(approve) {
    if (!orchActiveRunID) return;
    if (!orchPendingReqID) { closeInterventionModal(); return; }
    const errEl = el("orch-input-error");
    errEl.hidden = true;
    const guidance = el("orch-input-guidance").value.trim();
    try {
      const r = await fetch(
        "/api/v1/orchestrations/" + encodeURIComponent(orchActiveRunID) + "/respond",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          credentials: "same-origin",
          body: JSON.stringify({ req_id: orchPendingReqID, approve, guidance }),
        }
      );
      if (r.status === 202) {
        orchPendingReqID = null;
        el("orch-guide-btn").hidden = true;
        closeInterventionModal();
        return;
      }
      if (r.status === 409) {
        // Already resolved/timed out server-side; drop the modal and re-sync.
        orchPendingReqID = null;
        el("orch-guide-btn").hidden = true;
        closeInterventionModal();
        if (orchActiveRunID) refreshRunDetail(orchActiveRunID);
        return;
      }
      const j = await r.json().catch(() => ({}));
      errEl.textContent = j.error || ("Response failed (" + r.status + ").");
      errEl.hidden = false;
    } catch {
      errEl.textContent = "Response request failed. Is the server reachable?";
      errEl.hidden = false;
    }
  }

  function renderReport(rep) {
    const box = el("orch-report");
    box.innerHTML = "";
    box.hidden = false;
    const results = rep.results || [];

    const h = document.createElement("div");
    h.className = "orch-report-head";
    h.textContent = "Report — " + (rep.passed ? "PASSED" : "FAILED") +
      " · " + (rep.subtasks || 0) + " subtask(s)";
    box.appendChild(h);

    // Execution summary: passed/total, total iterations, duration, languages.
    const summary = document.createElement("div");
    summary.className = "orch-report-summary";
    const passedCount = results.filter((r) => r.passed).length;
    const totalIters = results.reduce((n, r) => n + (r.iterations || 0), 0);
    const parts = [`${passedCount}/${results.length} passed`, `${totalIters} iteration(s)`];
    const dur = fmtDuration(rep.duration_ms);
    if (dur) parts.push(dur);
    if (rep.languages && rep.languages.length) parts.push(rep.languages.join(", "));
    for (const p of parts) {
      const span = document.createElement("span");
      span.textContent = p;
      summary.appendChild(span);
    }
    box.appendChild(summary);

    for (const r of results) {
      const row = document.createElement("div");
      row.className = "orch-report-row";

      const b = document.createElement("strong");
      const sid = (r.subtask && r.subtask.id) || "";
      b.textContent = (r.branch ? "⑂ " + r.branch : sid) + " ";
      row.appendChild(b);
      const st = document.createElement("span");
      st.className = "pill " + (r.passed ? "online" : "offline");
      st.textContent = r.passed ? "pass" : "fail";
      row.appendChild(st);
      const rdur = fmtDuration(r.duration_ms);
      if (rdur) {
        const dEl = document.createElement("span");
        dEl.className = "muted";
        dEl.textContent = rdur;
        row.appendChild(dEl);
      }

      // Goal.
      const goal = r.subtask && r.subtask.goal;
      if (goal) {
        const gEl = document.createElement("div");
        gEl.className = "orch-subtask-goal";
        gEl.textContent = goal;
        row.appendChild(gEl);
      }

      // Acceptance criteria.
      const acc = (r.subtask && r.subtask.acceptance) || [];
      if (acc.length) {
        const lbl = document.createElement("div");
        lbl.className = "muted";
        lbl.textContent = "Acceptance:";
        row.appendChild(lbl);
        const ul = document.createElement("ul");
        ul.className = "orch-artifacts";
        for (const a of acc) {
          const li = document.createElement("li");
          li.textContent = "• " + a;
          ul.appendChild(li);
        }
        row.appendChild(ul);
      }

      // Testing done: Gate 1 results.
      const gates = r.gates || [];
      if (gates.length) {
        const lbl = document.createElement("div");
        lbl.className = "muted";
        lbl.textContent = "Testing done:";
        row.appendChild(lbl);
        for (const g of gates) {
          const line = document.createElement("div");
          line.className = "orch-step " + (g.passed ? "ok" : "err");
          line.textContent = (g.passed ? "✓ " : "✗ ") + (g.name || "gate");
          row.appendChild(line);
          if (!g.passed) {
            const tail = (g.err || g.output || "").slice(0, 300);
            if (tail) {
              const out = document.createElement("pre");
              out.className = "orch-gate-output";
              out.textContent = tail;
              row.appendChild(out);
            }
          }
        }
      }

      // Generated files + worktree location.
      const lbl = document.createElement("div");
      lbl.className = "muted";
      lbl.textContent = r.worktree_path
        ? "Generated in " + r.worktree_path
        : "Generated files";
      row.appendChild(lbl);
      const artifacts = r.artifacts || [];
      if (artifacts.length) {
        const ul = document.createElement("ul");
        ul.className = "orch-artifacts";
        for (const f of artifacts) {
          const li = document.createElement("li");
          li.textContent = f;
          ul.appendChild(li);
        }
        row.appendChild(ul);
      } else {
        const none = document.createElement("div");
        none.className = "muted";
        none.textContent = "(no file changes captured)";
        row.appendChild(none);
      }

      // Verdict notes / error.
      const notes = (r.verdict_notes || []).join("; ") || r.error || "";
      if (notes) {
        const notesEl = document.createElement("div");
        notesEl.className = "muted";
        notesEl.textContent = notes;
        row.appendChild(notesEl);
      }

      // Self-repair actions: recoverable env/infra failures the loop auto-fixed.
      const repairs = r.repairs || [];
      if (repairs.length) {
        const lbl = document.createElement("div");
        lbl.className = "muted";
        lbl.textContent = "Self-repair:";
        row.appendChild(lbl);
        const ul = document.createElement("ul");
        ul.className = "orch-artifacts";
        for (const rp of repairs) {
          const li = document.createElement("li");
          li.className = "orch-repair";
          const iter = rp.iteration ? "iter " + rp.iteration + ": " : "";
          const action = rp.action ? " — " + rp.action : "";
          li.textContent = "↻ " + iter + (rp.reason || rp.kind || "recovered") + action;
          ul.appendChild(li);
        }
        row.appendChild(ul);
      }

      // Interventions: authorization decisions / guidance the user supplied.
      const interventions = r.interventions || [];
      if (interventions.length) {
        const lbl = document.createElement("div");
        lbl.className = "muted";
        lbl.textContent = "Interventions:";
        row.appendChild(lbl);
        const ul = document.createElement("ul");
        ul.className = "orch-artifacts";
        for (const iv of interventions) {
          const li = document.createElement("li");
          const kind = iv.kind || "intervention";
          const decision = iv.approved ? "approved" : "denied";
          let text = "• " + kind + ": " + decision;
          if (iv.summary) text += " — " + iv.summary;
          if (iv.guidance) text += " (guidance: " + iv.guidance + ")";
          li.textContent = text;
          ul.appendChild(li);
        }
        row.appendChild(ul);
      }

      box.appendChild(row);
    }
  }

  async function viewDiff() {
    if (!orchActiveRunID) return;
    const diff = el("orch-diff");
    if (!diff.hidden) { diff.hidden = true; return; }
    try {
      const r = await fetch("/api/v1/orchestrations/" + encodeURIComponent(orchActiveRunID) + "/diff",
        { credentials: "same-origin" });
      const text = await r.text();
      diff.textContent = r.ok ? (text || "(no changes)") : ("diff failed: " + text);
      diff.hidden = false;
    } catch {
      diff.textContent = "diff request failed.";
      diff.hidden = false;
    }
  }

  // ---- Wire up --------------------------------------------------------------

  async function start() {
    const info = await loginInfo();
    provider = info.provider || "password";
    connStatus.textContent = "online";
    connStatus.className = "pill online";

    const res = await sessionsProbe();
    if (!res.ok && res.status === 401) {
      showLogin(info);
      return;
    }
    showApp();
    startPolling();
  }

  el("login-form").addEventListener("submit", doPasswordLogin);
  el("logout-btn").addEventListener("click", doLogout);
  el("refresh-btn").addEventListener("click", poll);
  el("detach-btn").addEventListener("click", detach);

  // ---- Mobile view wiring ---------------------------------------------------
  el("mobile-toggle").addEventListener("click", () => applyMobileMode(!mobileMode));
  el("mobile-back").addEventListener("click", () => closeMobileAction());
  // Delegated keystroke buttons: map named keys to escape sequences, else send
  // the raw single character carried on data-send.
  const MOBILE_KEYS = { up: "\x1b[A", down: "\x1b[B", enter: "\r", esc: "\x1b" };
  el("mobile-action").addEventListener("click", (e) => {
    const btn = e.target.closest("[data-send]");
    if (!btn) return;
    const k = btn.getAttribute("data-send");
    sendMobileInput(Object.prototype.hasOwnProperty.call(MOBILE_KEYS, k) ? MOBILE_KEYS[k] : k);
  });
  el("mobile-text-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const input = el("mobile-text");
    const v = input.value;
    if (v !== "") sendMobileInput(v + "\r");
    input.value = "";
  });
  el("spawn-cancel").addEventListener("click", closeSpawn);
  el("spawn-go").addEventListener("click", doSpawn);
  el("orch-new-btn").addEventListener("click", showOrchLaunch);
  el("orch-form").addEventListener("submit", startOrchestration);
  el("orch-diff-btn").addEventListener("click", viewDiff);
  // Intervention modal: approve (auth) / send-guidance (input) vs deny / skip.
  el("orch-input-approve").addEventListener("click", () => submitIntervention(true));
  el("orch-input-deny").addEventListener("click", () => submitIntervention(false));
  // The Guide button re-opens the pending intervention as a guidance prompt so
  // the user can inject steering text for the open request.
  el("orch-guide-btn").addEventListener("click", () => {
    if (!orchPendingReqID) return;
    openInterventionModal({
      mode: "input",
      reqId: orchPendingReqID,
      summary: "Add guidance for the paused run.",
      detail: "",
    });
  });

  start().catch(() => {
    connStatus.textContent = "offline";
    connStatus.className = "pill offline";
  });

  // Decide the initial view (phone vs desktop) once at load and start following
  // viewport changes when the user has no explicit stored preference.
  initMobileMode();
})();
