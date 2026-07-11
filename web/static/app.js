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

  let provider = "password";
  let pollTimer = null;
  let activeSessionID = null;
  let term = null;
  let fitAddon = null;
  let termSocket = null;
  let decoder = new TextDecoder();

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

  function renderDevices(groups) {
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

      for (const s of g.sessions) {
        const hb = s.heartbeat || {};
        const observed = s.kind === "observed" || s.attachable === false;
        const waiting = s.state === "waiting";
        const card = document.createElement("div");
        card.className = "session-card"
          + (observed ? " observed" : "")
          + (waiting ? " attention" : "")
          + (s.id === activeSessionID ? " active" : "");
        card.dataset.id = s.id;

        const row1 = document.createElement("div");
        row1.className = "row1";
        const cmd = document.createElement("span");
        cmd.className = "cmd";
        cmd.textContent = hb.command || s.id;
        row1.appendChild(cmd);
        // Observed processes are discovered read-only; tag them so the user
        // knows there is no interactive terminal (unlike managed sessions).
        if (observed) {
          const badge = document.createElement("span");
          badge.className = "badge observed";
          badge.title = "Discovered process (read-only monitor). Open a managed session for an interactive terminal.";
          badge.textContent = "observed";
          row1.appendChild(badge);
        }
        const st = document.createElement("span");
        st.className = stateClass(s.state);
        st.textContent = s.state || "idle";
        row1.appendChild(st);
        card.appendChild(row1);

        const meta = document.createElement("div");
        meta.className = "meta";
        const bits = [];
        if (hb.model) bits.push(hb.model);
        if (hb.branch) bits.push("⑂ " + hb.branch);
        if (observed && s.pid) bits.push("pid " + s.pid);
        if (hb.device_pinned) bits.push("📌");
        if (hb.cwd) bits.push(hb.cwd);
        meta.textContent = bits.join("  ·  ");
        card.appendChild(meta);

        if (observed) {
          // Read-only: no terminal attach, but clicking opens a metadata panel
          // so the user can inspect the discovered process.
          card.title = "Read-only monitor — click to inspect (no interactive terminal)";
          card.addEventListener("click", () => showObserved(s));
        } else {
          card.addEventListener("click", () => attach(s.id, hb));
        }
        wrap.appendChild(card);
      }
      list.appendChild(wrap);
    }
    processAttention(groups);
  }

  // ---- Intervention alert processing ----------------------------------------

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
        if (s.state === "waiting") {
          waiting.push({ id: s.id, device: g.device, hb: s.heartbeat || {} });
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

    // Fire a browser Notification once per transition into waiting.
    const nowWaiting = new Set(waiting.map((w) => w.id));
    for (const w of waiting) {
      if (!alertedIDs.has(w.id)) {
        alertedIDs.add(w.id);
        notify(w);
      }
    }
    // Drop ids that are no longer waiting so a later re-entry alerts again.
    for (const id of Array.from(alertedIDs)) {
      if (!nowWaiting.has(id)) alertedIDs.delete(id);
    }
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
    fitAddon.fit();

    term.onData((data) => {
      if (termSocket && termSocket.readyState === WebSocket.OPEN) {
        termSocket.send(JSON.stringify({ type: "input", data }));
      }
    });

    window.addEventListener("resize", () => {
      if (fitAddon) { fitAddon.fit(); sendResize(); }
    });
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

    el("empty-stage").hidden = true;
    el("observed-panel").hidden = true;
    el("terminal-panel").hidden = false;
    el("term-session").textContent = (hb && hb.command) || id;
    el("term-meta").textContent = id;

    ensureTerm();
    term.reset();
    fitAddon.fit();

    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/v1/terminal/${encodeURIComponent(id)}`;
    termSocket = new WebSocket(url);
    termSocket.binaryType = "arraybuffer";

    termSocket.onopen = () => { sendResize(); term.focus(); };
    termSocket.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        term.write(decoder.decode(new Uint8Array(ev.data)));
      } else {
        term.write(ev.data);
      }
    };
    termSocket.onclose = () => {
      if (activeSessionID === id) term.write("\r\n\x1b[90m[disconnected]\x1b[0m\r\n");
    };

    // Re-render list to highlight the active card.
    poll();
  }

  function detach() {
    if (termSocket) {
      try { termSocket.close(); } catch { /* ignore */ }
      termSocket = null;
    }
    activeSessionID = null;
    el("terminal-panel").hidden = true;
    el("observed-panel").hidden = true;
    el("empty-stage").hidden = false;
  }

  // ---- Observed (read-only) inspection --------------------------------------

  // showObserved renders a discovered process's metadata in the read-only panel.
  // Observed processes have no PTY, so there is deliberately no terminal here.
  function showObserved(s) {
    detach();
    activeSessionID = s.id;
    const hb = s.heartbeat || {};

    el("empty-stage").hidden = true;
    el("terminal-panel").hidden = true;
    el("observed-panel").hidden = false;
    el("obs-session").textContent = hb.command || s.id;
    el("obs-meta").textContent = s.id;

    const fields = [
      ["State", s.state || "idle"],
      ["PID", s.pid ? String(s.pid) : ""],
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
    const body = {
      device: spawnDevice,
      project_path: el("spawn-project").value,
      command: el("spawn-command").value.trim(),
      worktree_branch: el("spawn-branch").value.trim(),
      worktree_location: el("spawn-location").value,
    };
    try {
      const r = await fetch("/api/v1/spawn", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "same-origin",
        body: JSON.stringify(body),
      });
      if (r.status === 202) {
        closeSpawn();
        // Give the agent a moment to connect, then refresh.
        setTimeout(poll, 1500);
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
  el("spawn-cancel").addEventListener("click", closeSpawn);
  el("spawn-go").addEventListener("click", doSpawn);

  start().catch(() => {
    connStatus.textContent = "offline";
    connStatus.className = "pill offline";
  });
})();
