# Benchmarks and what they killed

Everything here was measured before it was claimed. Where a measurement went
against the tool, it is written down as it came out — including the two times it
went against a feature I had already built.

Repos under test are anonymised: **repo A** is a private TS/PHP monorepo
(5,079 indexable files), **repo B** an open TS/Python codebase (1,090 files).
The A/B task set is not published because it encodes private architecture; the
static bench and invariant harnesses are, and derive their queries from whatever
repo you point them at.

Tooling: `rg` 15.1.0, `rtk` 0.44.1, `claude` 2.1.220, `codex` 0.146.0,
rustc 1.93.1, 16 cores.

---

## 1. Static bench — output size and fidelity

3 runs per query, median latency.

| | rg | rtk 0.44.1 | codescout |
|---|---|---|---|
| **repo A**, 20 queries | 3,135,738 B | 261,566 B (12.0×) | **63,594 B (49.3×)** |
| def-recall | 100 % | **57.1 %** | **97.1 %** |
| latency p50 / p90 | 20 / 22 ms | 30 / 55 ms | 23 / 35 ms |
| **repo B**, 10 derived queries | 1,280,631 B | 150,870 B (8.5×) | **40,586 B (31.6×)** |
| def-recall | 100 % | **45.8 %** | **90.1 %** |
| latency p50 / p90 | 8 / 9 ms | 20 / 40 ms | 11 / 17 ms |

`def-recall` is judged by a regex run by `rg`, not by codescout's ranker.

On repo A, rtk loses **every** definition on 6 of the high-volume queries: it
cuts at 200 results taken in alphabetical order. And on the 7 low-volume
queries, `rtk rg` returns exactly as many bytes as `rg` — no compression at all.

## 2. Invariants — 200 real queries per repo, both repos

| Invariant | repo A | repo B |
|---|---|---|
| I1 passthrough: every line emitted is a real `rg` line | 8/8 patterns | 8/8 patterns |
| I2 never_worse: never larger than the faithful output | 0/200 | 0/200 |
| I3 `hits=` / `files=` counters exact | 0/200 | 0/200 |
| I4 attached source is byte-identical to the file | 0 mismatches / 1,177 lines | idem |

Output is deterministic: 3 runs byte-identical. Exit codes match `rg` (0/1/2).

Why passthrough is capped: `[a-z]{2,}` produces **72 MB** on repo A. Without a
bound, one query with no literal anchor destroys the agent's context.

## 3. Agentic A/B — the measurement that changed the design

Three arms with an identical tool surface, only the instruction differs
(native / rtk / codescout), on `claude -p` and `codex exec`, over tasks with a
verifiable answer.

**Round 1 — "which file defines X?" (single hop)**

| harness | arm | calls | **input tokens** | correct |
|---|---|---|---|---|
| claude | native | 1.67 | 118,152 | 6/6 |
| claude | rtk | 1.83 | 125,832 | 6/6 |
| claude | **codescout** | **1.00** | **88,748** | **6/6** |
| codex | native | 1.33 | 86,548 | 3/3 |
| codex | rtk | 1.67 | 105,433 | 3/3 |
| codex | **codescout** | **1.00** | **71,823** | **3/3** |

One call, one answer, in both harnesses — and the only case where input tokens
go down (−25 % claude, −17 % codex). rtk raises them in both.

**Round 2 — "what value?" (multi-hop)**

| harness | arm | calls | bytes read | worst case | **input tokens** |
|---|---|---|---|---|---|
| codex | native | 1.80 | 297,590 | 1,064,799 | **147,765** |
| codex | rtk | 3.40 | 29,943 | 39,366 | 202,967 |
| codex | codescout | 6.40 | **12,775** | **27,604** | 261,904 |

**This is the most important result of the campaign and it is against the
tool.** codescout reads **23× fewer bytes** than native — and triples the
round-trips, so total input tokens rise **77 %**. rtk takes the same hit (+37 %).

The lesson generalises past this project: in an agent loop, every turn re-bills
the whole conversation, so **bytes cost linearly and turns cost quadratically**.
Compressing a response only wins where the number of calls does not move.

Direct consequence: *"always start with scout"* — the instruction actually
tested — is what produces the regression. The instruction that works is
"identifier or definition → scout; reading and following a trail → native
tools".

## 4. Point A — built, measured, not shipped

The obvious follow-up was `scout read --around-line N`. That framing is wrong:
a subcommand the agent must *call* adds a round-trip, so it cannot save tokens.
The same code inlined into the search result — the top definition's source
attached to the response — *removes* one. That is what was built and tested
(`--body N`).

**Verdict: no measurable effect. Off by default.**

296 real runs, four arms. The two codescout arms received a **byte-identical
prompt** and differed only in a flag appended by a `PATH` shim, so any delta
between them is point A and nothing else. Paired by task and repetition, 95 % CI
by bootstrap (10,000 resamples):

| round | harness | n | Δ mean tokens | 95 % CI | signs | verdict |
|---|---|---|---|---|---|---|
| multi-hop | claude | 25 | −1,523 | [−20,435, +19,383] | 14↓/11↑ | indistinguishable from 0 |
| multi-hop | codex | 25 | −45,615 | [−100,631, +4,056] | 16↓/9↑ | indistinguishable from 0 |
| single-hop | claude | 12 | −48,896 | [−128,698, +121] | 6↓/6↑ | indistinguishable from 0 |
| single-hop | codex | 12 | +33,017 | [−7,004, +86,644] | 6↓/6↑ | indistinguishable from 0 |

The means flatter it; **the medians settle it**. Single-hop on claude: 85,814
tokens with the body against 86,318 without — **−0.6 %**. That −48,896 mean is
one runaway control run, not a gain.

### The first pass said the opposite, and it was wrong

At n=5 per cell, codex showed 3.00 calls against 4.80 and **−30 % tokens**. At
n=25 it collapses into the noise. Spread between two repetitions of the same
cell is **45,000–48,000 tokens**, against a between-arm gap of about 36,000:
**the noise is larger than the signal.** Resolving the codex multi-hop effect
would need n≈33 per arm — roughly 140 more codex runs — and would not change the
usage rule, so it was not run.

### What point A does win, and why it was not enough

- **Bytes**: still 3.5× more compact than rtk *while carrying whole function
  bodies* (4.1× without). Median bytes read on codex multi-hop: 11,256 against
  30,604 for rtk and 281,280 native.
- def-recall unchanged (96.9 % / 90.1 %), correctness unchanged (36/37 vs 35/37).

Bytes are not the deciding metric — that is the whole point of §3. A byte win
without a token win does not justify the complexity, so `--body` ships at 0.

## 5. Method errors, corrected, worth knowing

- **A whole A/B round was invalid**: the harness appended "answer with the file
  path" to questions asking for a value. `codex` obeyed the last instruction,
  silently turning a multi-hop round into a duplicate of round 1. Discarded and
  re-run.
- **The first invariant test compared raw bytes** while `rg` does not order files
  deterministically. Redone on line sets.
- **The benchmark repo changed under the campaign**: a 260-file package was
  deleted from repo A between two runs, killing 4 of 11 A/B tasks, and repo A is
  not under version control — the measured state is unrecoverable. Numbers from
  campaigns run on different repo states are therefore never mixed in one table;
  each campaign re-measures every arm, native and rtk included. Measure on a
  versioned tree, or record the commit.

## Kill criteria, written in advance

| Date | Condition | Action |
|---|---|---|
| 2026-11-01 | fewer than 30 invocations/week in real transcripts | uninstall, write the post-mortem |
| 2026-11-01 | one masked definition observed in real use without being announced | fix within 7 days or kill |
| 2026-11-01 | `--body` still unused by anyone | delete the flag |
| permanent | an invariant (I1–I4) falls and is not repaired within 7 days | kill |

## Frozen scope

One thing: `scout search`. No index, no daemon, no state on disk, one hard
dependency (`rg`). The day one of those four lines breaks, it is a different
project and it needs re-justifying.
