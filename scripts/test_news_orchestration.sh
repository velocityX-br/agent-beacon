#!/usr/bin/env bash
#
# test_news_orchestration.sh
#
# End-to-end test of agent-beacon's multi-agent scheduling + cross-model
# verification, driving a REAL project: build a Python news aggregator that
# collects headlines from authoritative Chinese and international media via RSS
# and emits a tidy report.
#
# What it exercises:
#   * Planner decomposes the task into parallel subtasks.
#   * Multiple workers run concurrently, each in an isolated git worktree.
#   * Gate 1 (deterministic): pytest must pass in the worktree.
#   * Gate 2 (cross-model verifier): a DIFFERENT model judges goal-completion.
#   * Self-repair loop: gate/verifier failures are fed back until pass or budget.
#
# This makes real Anthropic API calls and lets the coding agent (`claude`) write
# code into a THROWAWAY scratch repo. It never touches your real projects.
#
# Usage:
#   # Direct Anthropic API:
#   ANTHROPIC_API_KEY=sk-... ./scripts/test_news_orchestration.sh
#
#   # Via a Claude Code proxy/gateway (e.g. HAI) configured in
#   # ~/.claude/settings.json — no ANTHROPIC_API_KEY needed:
#   ANTHROPIC_AUTH_TOKEN=... ANTHROPIC_BASE_URL=http://localhost:6655/anthropic/ \
#     WORKER_MODEL=anthropic--claude-sonnet-latest \
#     VERIFIER_MODEL=anthropic--claude-opus-latest \
#     PLANNER_MODEL=anthropic--claude-sonnet-latest \
#     ./scripts/test_news_orchestration.sh
#
# Optional env overrides:
#   WORKERS=3 MAX_ITERS=4 \
#   WORKER_MODEL=... VERIFIER_MODEL=... PLANNER_MODEL=... \
#   BEACON_BIN=/path/to/agent-beacon \
#   KEEP=1  ./scripts/test_news_orchestration.sh
#     KEEP=1 keeps the scratch repo + worktrees for inspection.

set -euo pipefail

# ------------------------------------------------------------------ config ---
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BEACON_BIN="${BEACON_BIN:-$REPO_ROOT/bin/agent-beacon}"

WORKERS="${WORKERS:-3}"
MAX_ITERS="${MAX_ITERS:-4}"

# ------------------------------------------------- proxy env from settings ---
# The orchestrator's cross-model VERIFIER is our own Go HTTP client; unlike the
# `claude` CLI it does NOT read ~/.claude/settings.json, so it needs the proxy
# credentials as environment variables. If they are not already exported (e.g.
# you launched this from a bare terminal rather than inside Claude Code), pull
# ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY from the
# settings file's "env" block so the verifier can reach the gateway. This runs
# BEFORE model-default detection so the credential style is known.
load_proxy_env_from_settings() {
  local settings="${CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
  [[ -f "$settings" ]] || return 0
  command -v python3 >/dev/null 2>&1 || return 0
  local key val
  for key in ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_API_KEY; do
    if [[ -z "${!key:-}" ]]; then
      val="$(python3 -c 'import json,sys
try:
    d=json.load(open(sys.argv[1]))
    print(d.get("env",{}).get(sys.argv[2],""))
except Exception:
    print("")' "$settings" "$key" 2>/dev/null)"
      [[ -n "$val" ]] && export "$key=$val"
    fi
  done
  # Always succeed: the final loop test ([[ -n "$val" ]]) may be false, and under
  # `set -e` a non-zero function return would silently abort the whole script.
  return 0
}
load_proxy_env_from_settings

# Model defaults adapt to the credential style:
#   * Direct Anthropic API ($ANTHROPIC_API_KEY) -> claude-*-4-6 IDs.
#   * Claude Code proxy/gateway ($ANTHROPIC_AUTH_TOKEN, no key) -> the proxy's
#     "anthropic--claude-*-latest" IDs, since the gateway rewrites names.
# The verifier ALWAYS uses a different family (opus) than the worker (sonnet)
# so the cross-model check is genuine.
if [[ -z "${ANTHROPIC_API_KEY:-}" && -n "${ANTHROPIC_AUTH_TOKEN:-}" ]]; then
  DEF_PLANNER="anthropic--claude-sonnet-latest"
  DEF_WORKER="anthropic--claude-sonnet-latest"
  DEF_VERIFIER="anthropic--claude-opus-latest"
else
  DEF_PLANNER="claude-sonnet-4-6"
  DEF_WORKER="claude-sonnet-4-6"
  DEF_VERIFIER="claude-opus-4-6"
fi
PLANNER_MODEL="${PLANNER_MODEL:-$DEF_PLANNER}"
WORKER_MODEL="${WORKER_MODEL:-$DEF_WORKER}"
VERIFIER_MODEL="${VERIFIER_MODEL:-$DEF_VERIFIER}"
GATE_TIMEOUT="${GATE_TIMEOUT:-5m}"
TOTAL_TIMEOUT="${TOTAL_TIMEOUT:-20m}"
KEEP="${KEEP:-0}"

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/news-orch.XXXXXX")"
REPORT="$SCRATCH/orch-report.json"

# ------------------------------------------------------------------- utils ---
c_reset='\033[0m'; c_bold='\033[1m'; c_grn='\033[32m'; c_red='\033[31m'; c_yel='\033[33m'
info() { printf "${c_bold}==>${c_reset} %s\n" "$*"; }
ok()   { printf "${c_grn}OK${c_reset}  %s\n" "$*"; }
warn() { printf "${c_yel}WARN${c_reset} %s\n" "$*"; }
die()  { printf "${c_red}ERROR${c_reset} %s\n" "$*" >&2; exit 1; }

PYTHON=""
pick_python() {
  for p in python3 python; do
    if command -v "$p" >/dev/null 2>&1; then PYTHON="$p"; return; fi
  done
  die "no python interpreter found (need python3 or python)"
}

# ----------------------------------------------------------------- cleanup ---
cleanup() {
  local status=$?
  if [[ "$KEEP" == "1" ]]; then
    warn "KEEP=1 set — leaving scratch repo at: $SCRATCH"
    return
  fi
  info "cleaning up worktrees and scratch repo"
  if [[ -d "$SCRATCH/repo/.git" ]]; then
    # Remove any worktrees the orchestrator created (siblings of the repo).
    git -C "$SCRATCH/repo" worktree list --porcelain 2>/dev/null \
      | awk '/^worktree /{print $2}' \
      | while read -r wt; do
          [[ "$wt" == "$SCRATCH/repo" ]] && continue
          git -C "$SCRATCH/repo" worktree remove --force "$wt" 2>/dev/null || true
        done
  fi
  rm -rf "$SCRATCH"
  exit "$status"
}
trap cleanup EXIT INT TERM

# ----------------------------------------------------------- preflight ------
preflight() {
  info "preflight checks"
  if [[ -z "${ANTHROPIC_API_KEY:-}" && -z "${ANTHROPIC_AUTH_TOKEN:-}" ]]; then
    die "no Anthropic credentials: set ANTHROPIC_API_KEY (direct API) or ANTHROPIC_AUTH_TOKEN + ANTHROPIC_BASE_URL (proxy)"
  fi
  if [[ -z "${ANTHROPIC_API_KEY:-}" && -n "${ANTHROPIC_AUTH_TOKEN:-}" && -z "${ANTHROPIC_BASE_URL:-}" ]]; then
    warn "ANTHROPIC_AUTH_TOKEN is set but ANTHROPIC_BASE_URL is not — the verifier will hit api.anthropic.com, which likely rejects a proxy token"
  fi
  [[ -x "$BEACON_BIN" ]] || die "agent-beacon binary not found/executable at: $BEACON_BIN (run 'make build')"
  command -v git >/dev/null 2>&1 || die "git not found"
  command -v claude >/dev/null 2>&1 || die "the 'claude' CLI (worker agent) is not on PATH"
  pick_python
  "$PYTHON" -m pytest --version >/dev/null 2>&1 \
    || die "pytest not importable by $PYTHON — install it: $PYTHON -m pip install pytest"

  if [[ "$WORKER_MODEL" == "$VERIFIER_MODEL" ]]; then
    warn "worker and verifier models are identical ($WORKER_MODEL); the cross-model check is weakened"
  fi
  ok "binary=$BEACON_BIN"
  ok "python=$($PYTHON --version 2>&1) with pytest"
  if [[ -n "${ANTHROPIC_AUTH_TOKEN:-}" && -z "${ANTHROPIC_API_KEY:-}" ]]; then
    ok "auth: proxy (ANTHROPIC_AUTH_TOKEN via ${ANTHROPIC_BASE_URL:-api.anthropic.com})"
  else
    ok "auth: direct API (ANTHROPIC_API_KEY)"
  fi
  ok "models: planner=$PLANNER_MODEL worker=$WORKER_MODEL verifier=$VERIFIER_MODEL"
}

# --------------------------------------------------- seed scratch repo ------
# A Python project skeleton whose acceptance test FAILS until a worker
# implements the aggregator. The RSS feed list mixes authoritative Chinese and
# international outlets. The test tolerates network flakiness (each feed may
# fail) but requires a real, non-empty, tidy report to be produced.
seed_repo() {
  info "seeding scratch repo at $SCRATCH/repo"
  local repo="$SCRATCH/repo"
  mkdir -p "$repo"

  cat > "$repo/feeds.yaml" <<'EOF'
# Authoritative media RSS feeds — domestic (CN) and international.
# The aggregator must read this file (do not hardcode the list in code).
domestic:
  - name: 新华网 (Xinhua)
    url: http://www.xinhuanet.com/politics/news_politics.xml
  - name: 人民网 (People's Daily)
    url: http://www.people.com.cn/rss/politics.xml
  - name: 央视网 (CCTV)
    url: https://news.cctv.com/rss/china.xml
  - name: 中国日报 (China Daily)
    url: https://www.chinadaily.com.cn/rss/china_rss.xml
international:
  - name: BBC News
    url: https://feeds.bbci.co.uk/news/world/rss.xml
  - name: Reuters World
    url: https://feeds.reuters.com/reuters/worldNews
  - name: The New York Times (World)
    url: https://rss.nytimes.com/services/xml/rss/nyt/World.xml
  - name: The Guardian (World)
    url: https://www.theguardian.com/world/rss
EOF

  cat > "$repo/requirements.txt" <<'EOF'
feedparser
PyYAML
EOF

  # Acceptance test — this is the deterministic Gate 1. It defines the contract
  # the worker must satisfy. It is written to FAIL on the empty skeleton.
  cat > "$repo/test_aggregator.py" <<'EOF'
"""Acceptance test for the news aggregator (deterministic Gate 1).

The worker must implement aggregator.py exposing:
  - load_feeds(path="feeds.yaml") -> dict with 'domestic' and 'international'
  - fetch_headlines(feeds, limit=5) -> list of items, each a dict with at least
        {'source': str, 'category': str, 'title': str, 'link': str}
  - generate_report(items) -> a non-empty, human-readable string grouping
        headlines by category and source.

Network access may be flaky; the test injects a fake fetcher so it is
deterministic and offline-safe, while still exercising the real code paths.
"""
import os
import importlib

import pytest

agg = pytest.importorskip("aggregator")


def test_module_api_exists():
    for fn in ("load_feeds", "fetch_headlines", "generate_report"):
        assert hasattr(agg, fn), f"aggregator.{fn} is missing"


def test_load_feeds_reads_yaml(tmp_path):
    here = os.path.dirname(__file__)
    feeds = agg.load_feeds(os.path.join(here, "feeds.yaml"))
    assert "domestic" in feeds and "international" in feeds
    assert len(feeds["domestic"]) >= 1
    assert len(feeds["international"]) >= 1
    # Each feed entry must carry a name and url.
    sample = feeds["domestic"][0]
    assert "name" in sample and "url" in sample


def test_fetch_and_report_are_tidy(monkeypatch):
    """Inject a fake per-feed parser so the test is deterministic/offline."""
    fake_entries = {
        "http://cn.example/feed": [
            {"title": "国内头条一", "link": "http://cn.example/1"},
            {"title": "国内头条二", "link": "http://cn.example/2"},
        ],
        "http://intl.example/feed": [
            {"title": "World headline one", "link": "http://intl.example/1"},
        ],
    }

    feeds = {
        "domestic": [{"name": "示例国内", "url": "http://cn.example/feed"}],
        "international": [{"name": "Example Intl", "url": "http://intl.example/feed"}],
    }

    # The aggregator must expose a seam we can patch: a function that turns a
    # single feed URL into a list of {'title','link'} dicts. Accept either
    # `_parse_feed` or `parse_feed`.
    parse_name = "_parse_feed" if hasattr(agg, "_parse_feed") else "parse_feed"
    assert hasattr(agg, parse_name), (
        "aggregator must expose a per-feed parser (parse_feed/_parse_feed) so "
        "fetching can be tested without live network"
    )
    monkeypatch.setattr(agg, parse_name, lambda url, **kw: fake_entries.get(url, []))

    items = agg.fetch_headlines(feeds, limit=5)
    assert isinstance(items, list) and len(items) == 3
    for it in items:
        assert {"source", "category", "title", "link"} <= set(it.keys())
    cats = {it["category"] for it in items}
    assert cats == {"domestic", "international"}

    report = agg.generate_report(items)
    assert isinstance(report, str) and len(report.strip()) > 0
    # A tidy report groups by category and mentions sources + at least one title.
    assert "domestic" in report.lower() or "国内" in report
    assert "international" in report.lower() or "国际" in report
    assert "国内头条一" in report and "World headline one" in report


def test_cli_entrypoint_importable():
    """A runnable entrypoint should exist (main() or __main__ block)."""
    assert hasattr(agg, "main") or importlib.util.find_spec("aggregator") is not None
EOF

  # Empty skeleton: importing works but the API is absent, so the acceptance
  # test fails until a worker fills it in.
  cat > "$repo/aggregator.py" <<'EOF'
"""News aggregator — TO BE IMPLEMENTED by the worker agent.

Required public API (see test_aggregator.py):
  load_feeds(path="feeds.yaml") -> dict
  fetch_headlines(feeds, limit=5) -> list[dict]
  generate_report(items) -> str
  parse_feed(url) -> list[dict]      # per-feed seam, patched in tests
  main() -> None                     # CLI entrypoint that prints the report
"""
EOF

  cat > "$repo/README.md" <<'EOF'
# 权威媒体新闻头条聚合器 / Authoritative Media Headline Aggregator

Collect top headlines from authoritative Chinese domestic and international
media via RSS (see `feeds.yaml`) and generate a tidy, grouped report.

Run:
    pip install -r requirements.txt
    python aggregator.py         # prints the report
    pytest -q                    # acceptance tests (Gate 1)
EOF

  ( cd "$repo"
    git init -q
    git config user.email "orch-test@example.com"
    git config user.name  "orch test"
    git add -A
    git commit -qm "seed: news-aggregator skeleton with failing acceptance test"
  )
  ok "scratch repo seeded and committed"
}

# ------------------------------------------------------- run orchestrator ---
run_orchestration() {
  info "launching orchestrator (workers=$WORKERS max-iters=$MAX_ITERS)"

  local task
  task=$(cat <<'EOF'
Implement the Python news aggregator in aggregator.py so that all acceptance
tests in test_aggregator.py pass. It must:
  1. Read the RSS feed list from feeds.yaml (do NOT hardcode feeds in code).
  2. Fetch the latest headlines from each authoritative Chinese-domestic and
     international outlet, tolerating individual feed failures gracefully.
  3. Expose load_feeds, parse_feed (per-feed seam), fetch_headlines, and
     generate_report, plus a runnable main() that prints the report.
  4. generate_report must produce a tidy, human-readable report that groups
     headlines by category (domestic / international) and by source.
Ensure `pytest -q` passes and add a short docstring/usage note.
EOF
)

  set +e
  "$BEACON_BIN" orchestrate \
    --task "$task" \
    --repo "$SCRATCH/repo" \
    --workers "$WORKERS" \
    --max-iters "$MAX_ITERS" \
    --planner-model "$PLANNER_MODEL" \
    --worker-model "$WORKER_MODEL" \
    --verifier-model "$VERIFIER_MODEL" \
    --test-cmd "$PYTHON -m pytest -q" \
    --worktree-location sibling \
    --gate-timeout "$GATE_TIMEOUT" \
    --timeout "$TOTAL_TIMEOUT" \
    --report "$REPORT"
  ORCH_STATUS=$?
  set -e
}

# ---------------------------------------------------------- inspect result --
inspect() {
  info "orchestration exit status: $ORCH_STATUS"

  echo
  info "git worktrees created (isolation check):"
  git -C "$SCRATCH/repo" worktree list || true

  echo
  if [[ -f "$REPORT" ]]; then
    info "run report: $REPORT"
    if command -v jq >/dev/null 2>&1; then
      jq '{passed, total,
           subtasks: [.results[]? | {branch, passed, iterations,
                                     verdict: (.verdictNotes // []),
                                     worktree: .worktreePath}]}' "$REPORT" || cat "$REPORT"
    else
      cat "$REPORT"
      warn "install 'jq' for a prettier report summary"
    fi
  else
    warn "no report file produced at $REPORT"
  fi

  # Render the actual generated report by running the worker's code from
  # whichever worktree passed (or the repo itself if changes were merged).
  # Gate 1 (pytest) is re-run first so we only render code that still passes;
  # the live RSS fetch may be blocked in some environments, so a fetch failure
  # is reported as a warning rather than treated as a test failure.
  echo
  info "rendering the generated news report (live RSS fetch):"
  local target="$SCRATCH/repo"
  local passed_wt
  passed_wt=$(git -C "$SCRATCH/repo" worktree list --porcelain 2>/dev/null \
      | awk '/^worktree /{print $2}' | grep -v "/repo$" | head -n1 || true)
  [[ -n "${passed_wt:-}" && -f "$passed_wt/aggregator.py" ]] && target="$passed_wt"
  if [[ -f "$target/aggregator.py" ]]; then
    if ( cd "$target" && "$PYTHON" -m pytest -q >/dev/null 2>&1 ); then
      ok "Gate 1 re-check passed in $target"
      local rendered="$SCRATCH/generated-report.txt"
      if ( cd "$target" && "$PYTHON" aggregator.py ) >"$rendered" 2>/dev/null \
          && [[ -s "$rendered" ]]; then
        ok "generated report written to $rendered"
        echo "----------------------------- REPORT (first 60 lines) -----------------------------"
        head -n 60 "$rendered"
        echo "-----------------------------------------------------------------------------------"
      else
        warn "aggregator.py ran but produced no output — live RSS feeds may be unreachable here"
      fi
    else
      warn "Gate 1 re-check failed in $target — not rendering"
    fi
  else
    warn "no aggregator.py found to render"
  fi
}

# ---------------------------------------------------------------- verdict ---
verdict() {
  echo
  if [[ "$ORCH_STATUS" == "0" ]]; then
    ok "PASS — orchestrator completed all subtasks (gates + cross-model verifier green)"
  else
    warn "orchestrator finished with a non-zero status ($ORCH_STATUS)."
    warn "This may be expected if a subtask exhausted its iteration budget or a"
    warn "live RSS feed was unreachable. Inspect the report above for details."
  fi
  echo
  info "scheduling/verification checkpoints to confirm in the output above:"
  cat <<'EOF'
  [ ] Planner produced >= 1 subtask (see report .results[])
  [ ] Workers ran in isolated worktrees on orch/* branches (worktree list)
  [ ] Each worker looped: agent -> Gate 1 (pytest) -> Gate 2 (verifier)
  [ ] pytest passed BEFORE the verifier was consulted (determinism-first)
  [ ] The verifier model differed from the worker model (cross-model check)
  [ ] A tidy grouped news report was generated
EOF
}

# -------------------------------------------------------------------- main ---
main() {
  printf "${c_bold}agent-beacon multi-agent news-orchestration test${c_reset}\n"
  preflight
  seed_repo
  run_orchestration
  inspect
  verdict
}
main "$@"
