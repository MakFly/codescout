#!/usr/bin/env python3
"""Compare rg / rtk / scout on the same queries, same repo.

Three things are measured, in this order of importance:
  1. def-recall  — of the files an INDEPENDENT oracle says hold a definition of
                   the query, how many does the tool actually show? A tool that
                   is small because it dropped the answer is worthless.
  2. output size — bytes the agent would have to read.
  3. latency     — median of 3 runs.

The oracle is a plain regex run by rg, not scout's own ranker, so scout cannot
grade its own homework.
"""
import json
import re
import statistics
import subprocess
import sys
import time
from pathlib import Path

import os

SCOUT = os.environ.get("SCOUT_BIN", "scout")
RTK = os.environ.get("RTK_BIN", "rtk")

# Queries are derived from the repo under test (see `derive_queries`), so this
# bench runs on any codebase without carrying someone else's identifiers.
QUERIES = None

DEF_KW = r"(?:function|class|interface|type|enum|struct|impl|trait|fn|def|func|const|let|var|export\s+default)"


def run(cmd, cwd):
    t0 = time.perf_counter()
    p = subprocess.run(cmd, cwd=cwd, capture_output=True)
    return p.stdout, (time.perf_counter() - t0) * 1000


def timed(cmd, cwd, reps=3):
    outs, times = [], []
    for _ in range(reps):
        o, ms = run(cmd, cwd)
        outs.append(o)
        times.append(ms)
    return outs[0], statistics.median(times)


def oracle_def_files(q, cwd):
    """Files holding a definition of `q`, per an independent regex."""
    pat = rf"{DEF_KW}\s+(?:\w+\.)?{re.escape(q)}\b"
    p = subprocess.run(["rg", "-l", "-e", pat, "."], cwd=cwd, capture_output=True, text=True)
    return {l.lstrip("./") for l in p.stdout.splitlines() if l.strip()}


PATH_RE = re.compile(r"([\w./@-]+\.\w{1,5}):(\d+)")


def files_in(text):
    return {m.group(1).lstrip("./") for m in PATH_RE.finditer(text)}


def derive_queries(cwd, n_sym=10, n_hot=10):
    """Repo-agnostic query set: symbols the repo actually defines, plus its
    most frequent identifiers. Keeps the bench reproducible on any codebase."""
    pat = rf"(?:{DEF_KW})\s+([A-Za-z_][A-Za-z0-9_]{{5,24}})\b"
    p = subprocess.run(["rg", "-o", "--no-filename", "-e", pat, "-r", "$1", "."],
                       cwd=cwd, capture_output=True, text=True)
    from collections import Counter
    c = Counter(w for w in p.stdout.split() if w.isidentifier())
    syms = [w for w, n in c.most_common(300) if n <= 3][:n_sym]
    hot = [w for w, _ in c.most_common(n_hot * 3) if len(w) > 4][:n_hot]
    return syms + [h for h in hot if h not in syms]


def main(repo, queries=None):
    cwd = str(repo)
    rows = []
    for q in (queries or QUERIES):
        gt = oracle_def_files(q, cwd)

        rg_out, rg_ms = timed(["rg", "-n", "--no-heading", "-F", "-e", q, "."], cwd)
        rtk_out, rtk_ms = timed([RTK, "rg", "-n", q, "."], cwd)
        # Two scout arms so point A (source of the top definition attached to
        # the result) is isolated from everything else scout does.
        # Both flags are explicit: `--body` defaults to 0 since the point A
        # campaign, and a bench arm must not silently follow a default change.
        idx_out, idx_ms = timed([SCOUT, "search", q, ".", "--body", "0"], cwd)
        sc_out, sc_ms = timed([SCOUT, "search", q, ".", "--body", "30"], cwd)

        rg_t = rg_out.decode("utf8", "replace")
        rtk_t = rtk_out.decode("utf8", "replace")
        idx_t = idx_out.decode("utf8", "replace")
        sc_t = sc_out.decode("utf8", "replace")

        rg_files = files_in(rg_t)
        # Only score recall on definitions rg itself surfaces, so a bad oracle
        # regex cannot punish a tool for something rg never matched.
        gt = gt & rg_files
        rtk_files = files_in(rtk_t)
        idx_files = files_in(idx_t)
        sc_files = files_in(sc_t)

        rows.append({
            "q": q,
            "hits": len(rg_t.splitlines()),
            "gt_defs": len(gt),
            "rg_bytes": len(rg_out), "rtk_bytes": len(rtk_out),
            "scoutidx_bytes": len(idx_out), "scout_bytes": len(sc_out),
            "rg_ms": rg_ms, "rtk_ms": rtk_ms, "scoutidx_ms": idx_ms, "scout_ms": sc_ms,
            "rtk_recall": (len(gt & rtk_files) / len(gt)) if gt else None,
            "scoutidx_recall": (len(gt & idx_files) / len(gt)) if gt else None,
            "scout_recall": (len(gt & sc_files) / len(gt)) if gt else None,
            "rtk_missed": sorted(gt - rtk_files)[:3],
            "scout_missed": sorted(gt - sc_files)[:3],
            "body_bytes": len(sc_out) - len(idx_out),
        })
        print(f"  {q:22} hits={rows[-1]['hits']:>6}  defs={len(gt):>3}  "
              f"rg={len(rg_out):>8}  rtk={len(rtk_out):>7}  scout-idx={len(idx_out):>6}"
              f"  scout+body={len(sc_out):>6}", flush=True)

    print()
    tot = {k: sum(r[k] for r in rows)
           for k in ("rg_bytes", "rtk_bytes", "scoutidx_bytes", "scout_bytes")}
    print(f"TOTAL octets   rg={tot['rg_bytes']}  rtk={tot['rtk_bytes']}  "
          f"scout-idx={tot['scoutidx_bytes']}  scout+body={tot['scout_bytes']}")
    print(f"  rtk        : {tot['rg_bytes']/max(tot['rtk_bytes'],1):.1f}x plus compact que rg")
    print(f"  scout-idx  : {tot['rg_bytes']/max(tot['scoutidx_bytes'],1):.1f}x vs rg, "
          f"{tot['rtk_bytes']/max(tot['scoutidx_bytes'],1):.1f}x vs rtk")
    print(f"  scout+body : {tot['rg_bytes']/max(tot['scout_bytes'],1):.1f}x vs rg, "
          f"{tot['rtk_bytes']/max(tot['scout_bytes'],1):.1f}x vs rtk   "
          f"(corps = +{tot['scout_bytes']-tot['scoutidx_bytes']} o, "
          f"{(tot['scout_bytes']/max(tot['scoutidx_bytes'],1)-1)*100:+.0f}%)")
    withbody = [r for r in rows if r["body_bytes"] > 0]
    print(f"  corps attaché sur {len(withbody)}/{len(rows)} requêtes, "
          f"médiane {statistics.median([r['body_bytes'] for r in withbody]) if withbody else 0:.0f} o")

    for tool in ("rtk", "scoutidx", "scout"):
        rec = [r[f"{tool}_recall"] for r in rows if r[f"{tool}_recall"] is not None]
        perfect = sum(1 for x in rec if x == 1.0)
        print(f"  {tool:5} def-recall moyen = {statistics.mean(rec)*100:.1f}%  "
              f"({perfect}/{len(rec)} requêtes sans perte)")

    for tool in ("rg", "rtk", "scoutidx", "scout"):
        ts = sorted(r[f"{tool}_ms"] for r in rows)
        p50 = statistics.median(ts)
        p90 = ts[int(len(ts) * 0.9)]
        print(f"  {tool:5} latence p50={p50:.0f}ms p90={p90:.0f}ms max={ts[-1]:.0f}ms")

    out = Path(__file__).parent / f"results-{repo.name}.json"
    out.write_text(json.dumps(rows, indent=2))
    print(f"\n-> {out}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit("usage: bench.py <repo> [query ...]\n"
                 "  no queries given -> derived from the repo itself\n"
                 "  SCOUT_BIN / RTK_BIN override the binaries used")
    repo = Path(sys.argv[1])
    explicit = sys.argv[2:]
    main(repo, explicit or derive_queries(str(repo)))
