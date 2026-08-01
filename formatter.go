// Fills a token budget with the highest-ranked evidence, and states plainly
// what it did not show. Counters are always the true ones; nothing is silently
// dropped and no synthetic marker is ever injected into source text.

package main

import (
	"fmt"
	"math"
	"strings"
)

const displayMax = 150

// Code sits around 3.3 characters per token; bytes/4 (what some tools use)
// overestimates savings on source lines.
const charsPerToken = float32(3.3)

func estTokens(s string) int {
	return int(math.Round(float64(float32(runeLen(s)) / charsPerToken)))
}

func clip(s string) string {
	out, cut := takeRunes(s, displayMax)
	if cut {
		return out + "…"
	}
	return out
}

func entry(f *FileHit, explains map[string]Explain) string {
	var l *Line
	if f.Def != nil {
		l = f.Def
	} else if len(f.Best) > 0 {
		l = &f.Best[0]
	}
	var s strings.Builder
	if l == nil {
		fmt.Fprintf(&s, "%s  (%dx)\n", f.Path, f.Matches)
		return s.String()
	}
	fmt.Fprintf(&s, "%s:%d", f.Path, l.N)
	if f.Matches > 1 {
		fmt.Fprintf(&s, "  (%dx)", f.Matches)
	}
	if l.IsDef {
		s.WriteString("  [def]")
	}
	if explains != nil {
		if e, ok := explains[f.Path]; ok {
			fmt.Fprintf(&s, "  {score=%.2f line=%.2f cov=%.2f dens=%.2f path=%.2f name=%.2f dedup=%.2f}",
				f.Score, e.Line, e.Coverage, e.Density, e.PathPrior, e.NameBonus, e.Dedup)
		}
	}
	s.WriteString("\n    ")
	s.WriteString(clip(l.Text))
	s.WriteString("\n")
	return s.String()
}

type RenderOpts struct {
	Query string
	// The literal query matched nothing and we fell back to term coverage.
	Fallback     bool
	BudgetTokens int
	Page         int
	Repro        string
	ElapsedMs    int64
	Explains     map[string]Explain
}

func render(out *Outcome, opts RenderOpts) string {
	if out.TotalMatch == 0 {
		return fmt.Sprintf("query: %s   hits=0 files=0\nrepro: %s\n", opts.Query, opts.Repro)
	}

	// Header and footer are part of the budget, so reserve for them.
	budgetChars := int(float32(opts.BudgetTokens) * charsPerToken)
	usable := budgetChars - 260
	if usable < 200 {
		usable = 200
	}

	// Walk the ranked list page by page; a page is full when the next entry
	// would cross the budget. Deterministic, so --page N is stable.
	type rng struct{ from, to int }
	var pages []rng
	start, used := 0, 0
	for i, f := range out.Files {
		n := runeLen(entry(f, opts.Explains))
		if used+n > usable && i > start {
			pages = append(pages, rng{start, i})
			start = i
			used = 0
		}
		used += n
	}
	pages = append(pages, rng{start, len(out.Files)})

	totalPages := len(pages)
	page := opts.Page
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	from, to := pages[page-1].from, pages[page-1].to

	var body strings.Builder
	for _, f := range out.Files[from:to] {
		body.WriteString(entry(f, opts.Explains))
	}

	shown := to - from
	remaining := len(out.Files) - to
	// Definitions are sorted to the front, so any that are off-page are on a
	// later page — the count is stated rather than left implicit.
	defsOffpage := 0
	for i, f := range out.Files {
		if (i < from || i >= to) && f.Def != nil {
			defsOffpage++
		}
	}

	var tail strings.Builder
	tail.WriteString("\n")
	if remaining > 0 {
		fmt.Fprintf(&tail,
			"+%d files not shown — scout search '%s' --page %d   (or --path/--lang/--kind symbol to narrow)\n",
			remaining, opts.Query, page+1)
	}
	if defsOffpage > 0 {
		fmt.Fprintf(&tail, "note: %d file(s) holding a definition are not on this page\n", defsOffpage)
	}
	fmt.Fprintf(&tail, "repro: %s\n", opts.Repro)

	warn := ""
	if opts.Fallback {
		// No indent on the second line: the reference uses a Rust line
		// continuation, which strips the leading whitespace of the wrapped line.
		warn = "note: no match for the literal string. Fell back to term coverage —\n" +
			"ranking is unreliable here, prefer an exact identifier or --kind symbol.\n"
	}
	header := fmt.Sprintf("query: %s   hits=%d files=%d shown=%d page=%d/%d   ~%dtok  %dms\n%s\n",
		opts.Query, out.TotalMatch, out.TotalFiles, shown, page, totalPages,
		estTokens(body.String())+estTokens(tail.String()), opts.ElapsedMs, warn)

	return header + body.String() + tail.String()
}
