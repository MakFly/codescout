# codescout — ranked code search for LLM agents

**`rg -n useState` on a real monorepo returns 186 KB — about 47,000 tokens, a
quarter of a context window for one search.** Truncating is the obvious fix and
the wrong one: alphabetical truncation throws away the file holding the answer
as readily as any other.

`codescout` runs [ripgrep](https://github.com/BurntSushi/ripgrep), ranks what
comes back, and spends a token budget on the best evidence — while keeping the
counters exact and **stating what it did not show**.

```
scout search useState .
query: useState   hits=1442 files=214 shown=31 page=1/7   ~1420tok  24ms

src/hooks/use-filters.ts:12  (18x)  [def]
    export function useFilters(initial: FilterState) {
src/components/table/data-table.tsx:44  (9x)
    const [rows, setRows] = useState<Row[]>([]);
...
+183 files not shown — scout search 'useState' --page 2
note: 4 file(s) holding a definition are not on this page
repro: rg -n --no-heading -F -e useState .
```

One Rust binary. **No index, no daemon, no state on disk.** One hard dependency:
`rg`.

---

## Why not just truncate ripgrep?

Because the tool that is small *because it dropped the answer* is worthless.
The metric that matters is **def-recall**: of the files an *independent* oracle
says hold a definition of the query, how many does the tool actually show?

Measured on two real codebases, 3 runs per query, against `rg` and
[`rtk`](https://github.com/rtk-rs) 0.44.1:

| | rg | rtk 0.44.1 | **codescout** |
|---|---|---|---|
| **Repo A** — private TS/PHP monorepo, 5,079 files, 20 queries | 3,135,738 B | 261,566 B (12.0×) | **63,594 B (49.3×)** |
| def-recall | 100 % | **57.1 %** | **97.1 %** |
| latency p50 / p90 | 20 / 22 ms | 30 / 55 ms | 23 / 35 ms |
| **Repo B** — 1,090 files, 10 derived queries | 1,280,631 B | 150,870 B (8.5×) | **40,586 B (31.6×)** |
| def-recall | 100 % | **45.8 %** | **90.1 %** |

**4.1× more compact than rtk, with 40 points more def-recall.** The oracle is a
plain regex run by `rg`, never codescout's own ranker — the tool does not grade
its own homework.

Reproduce it on your own code:

```bash
python3 bench/bench.py /path/to/your/repo     # queries derived from the repo
python3 bench/invariants.py /path/to/your/repo
```

## Install

**From a release** (Linux x86_64):

```bash
curl -fsSL https://github.com/MakFly/codescout/releases/latest/download/scout-x86_64-unknown-linux-gnu \
  -o ~/.local/bin/scout && chmod +x ~/.local/bin/scout
scout doctor
```

**From source** (needs Rust and `rg` on PATH):

```bash
cargo install --git https://github.com/MakFly/codescout
```

## Usage

```
scout search <query> [paths...]
       [--kind auto|literal|regex|symbol] [--path GLOB] [--lang ts]
       [--budget-tokens 1500] [--page N] [--body 0] [--json] [--explain] [-i]
scout doctor
```

### Use it for lookups, not for exploration

This is the single most important line in this README, and it is the result of
measurement, not taste.

| Question shape | Tool |
|---|---|
| "where is `createSession` defined?" — an identifier, a symbol, a definition | **`scout search`** |
| "how does the billing flow work?" — follow a trail, read a file, build a picture | **native `grep` / file reads** |

Across 296 real agent runs on `claude -p` and `codex exec`, codescout cut input
tokens by **25 %** on identifier lookups (1.00 tool call, versus 1.67 native and
1.83 for rtk). On multi-hop exploration it read **23× fewer bytes** — and input
tokens still went **up 77 %**, because it tripled the round-trips.

The mechanism is worth internalising, because it applies to every output
compressor in an agent loop: **bytes cost linearly, turns cost quadratically.**
Every turn re-bills the whole conversation, so shrinking a response is a linear
saving and adding a turn is a quadratic cost. Compression only wins where the
number of calls does not move.

So: telling an agent *"always start with scout"* is a measured regression.
The instruction that works is the table above.

## The four invariants

Structural rules, not heuristics. They are tested (`bench/invariants.py`) and a
violation is a blocking defect.

| # | Invariant | Verified |
|---|---|---|
| **I1** | A query with no 3-character literal anchor is not ranked — it passes through to `rg` verbatim. Every line emitted is a real `rg` line. | 8/8 patterns, exact subset |
| **I2** | `never_worse` — the ranked output is never larger than the faithful one. If it would be, you get the faithful one. | 0/200 violations |
| **I3** | `hits=` and `files=` are always the true counts, even when the display is partial. | 0/200 errors |
| **I4** | Every line of an attached `── source` block is byte-identical to the real file line it claims to quote. | 1,177 lines, 0 mismatches |

Corollary of I1: passthrough is **bounded** (`--passthrough-cap`, 100 KB).
`[a-z]{2,}` produces 18 MB on a mid-size repo; handing that to an agent is worse
than any ranking mistake. The cut is announced on stderr with the command to see
the rest.

## Known limits, written up front

- **Natural-language queries are the weak spot.** `scout search "seller listing
  quota limit"` matches nothing literally. codescout falls back to term coverage,
  which restores recall but ranks poorly — and **says so in its header** instead
  of pretending. Use an identifier.
- **No real symbol table.** Definition detection is a lexical heuristic (keyword
  before the identifier, not crossing a `=`). It catches TS/JS, Go, Rust, Python,
  PHP, C. It is not a parser; a true `find_references` needs tree-sitter, and
  that is not this version.
- **Token counts are estimates** (3.3 chars/token), announced as `~Ntok`.
- **Above ~2 GB of source**, cold `rg` exceeds a second and codescout has nothing
  to compensate. `scout doctor` says so.
- **`--body N` is off by default.** Attaching the top definition's source looked
  like a way to remove the follow-up read; across 296 agent runs it moved input
  tokens by no resolvable amount on either harness. It stayed, opt-in and
  documented, rather than being shipped on a hunch. See
  [BENCHMARKS.md](BENCHMARKS.md).

## What it deliberately does not do

No trigram index, no daemon, no blob store, no PageRank, no call graph, no
file-read compression, no `PreToolUse` hook, no embeddings. Each was evaluated
and dropped on measurement, not intuition.

`rtk` stays ahead on compressing `git`, `docker`, test and lint output — the
majority of real Bash traffic, which codescout does not touch. **The two tools
are orthogonal, not substitutes.** Run both.

## License

MIT © Kévin Aubrée
