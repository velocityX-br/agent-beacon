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
      card.appendChild(row1);

      const meta = document.createElement("div");
      meta.className = "meta";
      meta.textContent = run.repo;
      card.appendChild(meta);

      card.addEventListener("click", () => openRun(run.id));
      list.appendChild(card);
    }
  }

  // ---- Launcher -------------------------------------------------------------

  function showOrchLaunch() {
    detach();
    closeOrchSocket();
    orchActiveRunID = null;
    el("empty-stage").hidden = true;
    el("observed-panel").hidden = true;
    el("terminal-panel").hidden = true;
    el("orch-run-panel").hidden = true;
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

    el("empty-stage").hidden = true;
    el("observed-panel").hidden = true;
    el("terminal-panel").hidden = true;
    el("orch-launch-panel").hidden = true;
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
})();
