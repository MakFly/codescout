// The product. Turns an unordered pile of matches into an ordered one.
//
// Two rules are structural, not heuristic:
//  1. a file holding a definition-shaped match always sorts above files that do
//     not, whatever its path prior — a definition is never buried;
//  2. every score component is reproducible and dumpable via --explain.

package main

import (
	"math"
	"math/bits"
	"sort"
	"strings"
	"unicode"
)

// Words that introduce a definition. Kept deliberately small: the backward scan
// below is what gives precision, not the size of this list.
var defKW = map[string]bool{
	"function": true, "fn": true, "def": true, "class": true, "interface": true,
	"type": true, "enum": true, "struct": true, "impl": true, "trait": true,
	"const": true, "let": true, "var": true, "export": true, "default": true,
	"public": true, "private": true, "protected": true, "static": true,
	"async": true, "func": true, "module": true, "namespace": true,
	"abstract": true, "override": true, "val": true, "object": true,
	"record": true, "protocol": true, "extension": true, "component": true,
	"procedure": true, "method": true, "macro_rules": true, "declare": true,
	"readonly": true,
}

// Characters that break the link between a definition keyword and an
// identifier. `=` is what stops `const [rows, setRows] = useState(...)` from
// being read as a definition of `useState`; `[` `]` `,` are deliberately absent
// so destructuring declarations (`const [rows, setRows] = …` seen from
// `setRows`) are still recognised.
const breakers = "=(){};:?|&<>"

func isIdentChar(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '$'
}

type span struct{ start, end int }

// Byte offsets of every whole-word occurrence of any token in `line`.
func tokenPositions(line string, tokens []string) []span {
	lower := strings.ToLower(line)
	b := []byte(lower)
	var out []span
	for _, t := range tokens {
		if len(t) < 2 {
			continue
		}
		from := 0
		for from < len(lower) {
			rel := strings.Index(lower[from:], t)
			if rel < 0 {
				break
			}
			start := from + rel
			end := start + len(t)
			beforeOK := start == 0 || !isIdentChar(rune(b[start-1]))
			afterOK := end >= len(b) || !isIdentChar(rune(b[end]))
			if beforeOK && afterOK {
				out = append(out, span{start, end})
			}
			from = start + 1
		}
	}
	return out
}

// Walks backwards from an identifier looking for a definition keyword, stopping
// at the first breaker character.
func backwardDef(line string, start int) bool {
	if start > len(line) {
		start = len(line)
	}
	head := []rune(line[:start])
	var word []rune
	for i := len(head) - 1; i >= 0; i-- {
		c := head[i]
		if isIdentChar(c) {
			word = append([]rune{c}, word...)
			continue
		}
		if len(word) > 0 {
			if defKW[strings.ToLower(string(word))] {
				return true
			}
			word = nil
		}
		if strings.ContainsRune(breakers, c) {
			return false
		}
	}
	return len(word) > 0 && defKW[strings.ToLower(string(word))]
}

func firstWordIsDefKW(line string) bool {
	t := strings.TrimLeftFunc(line, unicode.IsSpace)
	var w strings.Builder
	for _, c := range t {
		if !isIdentChar(c) {
			break
		}
		w.WriteRune(c)
	}
	return defKW[strings.ToLower(w.String())]
}

func isComment(line string) bool {
	t := strings.TrimLeftFunc(line, unicode.IsSpace)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "/*") ||
		strings.HasPrefix(t, "--") || strings.HasPrefix(t, "<!--")
}

func isImport(line string) bool {
	t := strings.TrimLeftFunc(line, unicode.IsSpace)
	return strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "from ") ||
		strings.HasPrefix(t, "#include") || strings.HasPrefix(t, "use ") ||
		strings.HasPrefix(t, "require(") || strings.HasPrefix(t, "export * ") ||
		strings.HasPrefix(t, "export {")
}

// Scores one match line and says whether it looks like a definition.
func lineScore(line string, tokens []string) (float32, bool) {
	score := float32(1.0)
	positions := tokenPositions(line, tokens)
	wholeWord := len(positions) > 0

	isDef := false
	for _, p := range positions {
		if backwardDef(line, p.start) {
			isDef = true
			break
		}
		after := ""
		if p.end <= len(line) {
			after = strings.TrimLeftFunc(line[p.end:], unicode.IsSpace)
		}
		followedByParen := strings.HasPrefix(after, "(") || strings.HasPrefix(after, "<")
		// An assignment before the identifier means it is being *called*, not
		// declared: `const [rows, setRows] = useState(...)` defines rows, not
		// useState. Without this guard every React hook call reads as a
		// definition — measured: 296 false positives on a single query.
		assignedTo := strings.Contains(line[:min(p.start, len(line))], "=")
		if followedByParen && !assignedTo {
			if firstWordIsDefKW(line) {
				isDef = true
				break
			}
			// C-style / Go / class methods: `int main() {`, `handleClick() {`
			var before rune = 0
			if p.start > 0 {
				r := []rune(line[:p.start])
				if len(r) > 0 {
					before = r[len(r)-1]
				}
			}
			callLike := before == '.' || before == '>'
			if !callLike && strings.HasSuffix(strings.TrimRightFunc(line, unicode.IsSpace), "{") {
				isDef = true
				break
			}
		}
	}

	if isDef {
		score += 3.0
	}
	if wholeWord {
		score += 0.6
	}
	if runeLen(line) < 120 {
		score += 0.3
	}
	if isImport(line) {
		score -= 0.9
	}
	if isComment(line) {
		score -= 0.6
	}
	return score, isDef
}

type Explain struct {
	Line, Coverage, Density, PathPrior, NameBonus, Dedup float32
}

func pathPrior(path string) float32 {
	// Leading slash so the segment patterns below also match at path start.
	p := "/" + strings.ToLower(path)
	junk := []string{
		"/node_modules/", "/dist/", "/build/", "/.next/", "/out/", "/vendor/",
		"/coverage/", "/target/", ".min.", "/generated/", "/.venv/", "/__pycache__/",
	}
	testy := []string{
		"/test/", "/tests/", "/spec/", "__tests__", "__mocks__", ".test.", ".spec.",
		"/e2e/", "/cypress/", "/fixtures/", "/mocks/", ".stories.", "/storybook/",
	}
	for _, j := range junk {
		if strings.Contains(p, j) {
			return 0.10
		}
	}
	for _, t := range testy {
		if strings.Contains(p, t) {
			return 0.30
		}
	}
	if strings.HasSuffix(p, ".d.ts") {
		return 0.50
	}
	if strings.Contains(p, "/src/") || strings.Contains(p, "/app/") || strings.Contains(p, "/lib/") {
		return 1.10
	}
	return 1.0
}

func nameBonus(path string, tokens []string) float32 {
	file := strings.ToLower(path)
	if i := strings.LastIndex(file, "/"); i >= 0 {
		file = file[i+1:]
	}
	stem := file
	if i := strings.Index(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.NewReplacer("-", "", "_", "").Replace(stem)
	dir := strings.ToLower(path)
	for _, t := range tokens {
		if len(t) < 3 {
			continue
		}
		if strings.Contains(stem, t) {
			return 1.5
		}
	}
	for _, t := range tokens {
		if len(t) >= 4 && strings.Contains(dir, t) {
			return 0.7
		}
	}
	return 0.0
}

// Ranks files in place and returns the per-file score breakdown.
func rank(files []*FileHit, tokens []string, nTokens int) map[string]Explain {
	// Boilerplate detection: a representative line repeated across many files
	// (a shared import, a generated header) carries little information.
	repeats := map[string]int{}
	for _, f := range files {
		if len(f.Best) > 0 {
			repeats[f.Best[0].Text]++
		}
	}

	explains := map[string]Explain{}
	for _, f := range files {
		var line float32
		if f.Def != nil {
			line = f.Def.Score
		} else if len(f.Best) > 0 {
			line = f.Best[0].Score
		}
		// Coverage dominates on multi-term queries: a file mentioning four of
		// the five words asked about is what the agent wanted, even if each word
		// appears once. On a single-token query this term is constant.
		multi := nTokens > 1
		var coverage, density float32
		if multi {
			coverage = 3.0 * (float32(bits.OnesCount32(f.Covered)) / float32(nTokens))
		} else {
			// On a multi-term query raw match count is not relevance: a page
			// repeating a word 469 times outranked the actual module. What
			// discriminates there is the filename, not the volume.
			// f32 throughout, to match the reference implementation's rounding.
			density = float32(0.35 * float32(math.Log(float64(float32(1.0+float32(f.Matches))))))
			if density > 1.2 {
				density = 1.2
			}
		}
		prior := pathPrior(f.Path)
		// Filename match is the strongest signal available when the query is
		// prose, so it carries more weight there.
		name := nameBonus(f.Path, tokens)
		if multi {
			name *= 2.0
		}
		dedup := float32(1.0)
		if len(f.Best) > 0 && !f.Best[0].IsDef && repeats[f.Best[0].Text] > 3 {
			dedup = 0.6
		}
		f.Score = (line + coverage + density + name) * prior * dedup
		explains[f.Path] = Explain{line, coverage, density, prior, name, dedup}
	}

	// Hard key first: a file holding a definition never sorts below one that
	// does not. Path priors only order files within each group.
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		ad, bd := a.Def != nil, b.Def != nil
		if ad != bd {
			return ad
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Path < b.Path
	})
	return explains
}
