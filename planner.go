// Decides how a query is executed: literal, regex, or symbol — and whether
// scout should get out of the way entirely (passthrough).

package main

import (
	"strings"
	"unicode"
)

type Kind int

const (
	KindAuto Kind = iota
	KindLiteral
	KindRegex
	KindSymbol
)

type Plan struct {
	// Verbatim `rg` args, without the `--json` flag.
	RgArgs []string
	// True when the query has no literal substring of >= 3 chars. A trigram
	// index is 28x slower than rg on this class, and ranking a scan with no
	// anchor is guesswork — so we hand the query straight to rg.
	Passthrough bool
	// Identifier-ish tokens of the query, used by the ranker to detect
	// definition shapes. Empty for pure-regex queries.
	Tokens []string
	// Number of whitespace-separated words in the query. Coverage scoring only
	// applies when this is > 1.
	NTerms int
	// Whether definition detection is meaningful. A natural-language query
	// names no identifier, so nothing in it can be "defined" — and prose like
	// `type de vendeur` trips the `type` keyword.
	DetectDefs bool
}

const meta = `\^$.|?*+()[]{}`

func hasMeta(q string) bool {
	return strings.ContainsAny(q, meta)
}

func isMeta(c rune) bool {
	return strings.ContainsRune(meta, c)
}

// Longest run of characters that must appear verbatim in any match.
// Deliberately conservative: metacharacters end the current run, and the
// contents of `[...]` classes and `{...}` quantifiers count for nothing —
// `a-z` inside a class is not a literal anyone can search for.
func longestLiteral(q string) int {
	best, cur := 0, 0
	prevEscape := false
	depthClass, depthBrace := 0, 0
	for _, c := range q {
		if prevEscape {
			prevEscape = false
			cur = 0
			continue
		}
		switch {
		case c == '\\':
			prevEscape = true
			cur = 0
		case c == '[':
			depthClass++
			cur = 0
		case c == ']':
			if depthClass > 0 {
				depthClass--
			}
			cur = 0
		case c == '{':
			depthBrace++
			cur = 0
		case c == '}':
			if depthBrace > 0 {
				depthBrace--
			}
			cur = 0
		case depthClass > 0 || depthBrace > 0:
			cur = 0
		case isMeta(c):
			cur = 0
		default:
			cur++
			if cur > best {
				best = cur
			}
		}
	}
	return best
}

func contains(all []string, s string) bool {
	for _, a := range all {
		if a == s {
			return true
		}
	}
	return false
}

// Identifier-ish fragments, split on camelCase and non-alphanumerics.
// `useEditListingForm` -> ["useeditlistingform", "use", "edit", "listing", "form"]
func tokenize(q string) []string {
	var words []string
	var word strings.Builder
	for _, c := range q {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' {
			word.WriteRune(c)
		} else if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	if word.Len() > 0 {
		words = append(words, word.String())
	}

	all := []string{}
	for _, w := range words {
		lower := strings.ToLower(w)
		if len(lower) >= 2 && !contains(all, lower) {
			all = append(all, lower)
		}
		var part strings.Builder
		for _, c := range w {
			if unicode.IsUpper(c) && part.Len() > 0 {
				if part.Len() >= 3 {
					p := strings.ToLower(part.String())
					if !contains(all, p) {
						all = append(all, p)
					}
				}
				part.Reset()
			}
			part.WriteRune(c)
		}
		if part.Len() >= 3 {
			p := strings.ToLower(part.String())
			if !contains(all, p) {
				all = append(all, p)
			}
		}
	}
	return all
}

type PlanInput struct {
	Query      string
	Kind       Kind
	Paths      []string
	Globs      []string
	Lang       string
	IgnoreCase bool
	Hidden     bool
}

func plan(in PlanInput) Plan {
	kind := in.Kind
	if kind == KindAuto {
		if hasMeta(in.Query) {
			kind = KindRegex
		} else {
			kind = KindLiteral
		}
	}

	args := []string{"-n", "--no-heading"}
	switch kind {
	// -F keeps recall byte-identical to a plain `rg -n <query>`, which is what
	// the bench compares against. No smart-case, no word boundary.
	case KindLiteral:
		args = append(args, "-F")
	case KindSymbol:
		args = append(args, "-F", "-w")
	}
	if in.IgnoreCase {
		args = append(args, "-i")
	}
	if in.Hidden {
		args = append(args, "--hidden")
	}
	if in.Lang != "" {
		args = append(args, "-t", in.Lang)
	}
	for _, g := range in.Globs {
		args = append(args, "-g", g)
	}
	args = append(args, "-e", in.Query)
	args = append(args, in.Paths...)

	passthrough := kind == KindRegex && longestLiteral(in.Query) < 3
	var tokens []string
	if kind != KindRegex {
		tokens = tokenize(in.Query)
	}
	return Plan{
		RgArgs:      args,
		Passthrough: passthrough,
		Tokens:      tokens,
		NTerms:      len(terms(in.Query)),
		DetectDefs:  true,
	}
}

// Whitespace-separated words of at least 3 characters.
func terms(q string) []string {
	out := []string{}
	for _, w := range strings.Fields(q) {
		t := strings.ToLower(strings.TrimFunc(w, func(c rune) bool {
			return !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_'
		}))
		if len(t) >= 3 {
			out = append(out, t)
		}
	}
	return out
}

func escape(s string) string {
	var b strings.Builder
	for _, c := range s {
		if isMeta(c) {
			b.WriteRune('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Fallback for natural-language queries. A multi-word query matched literally
// is almost always zero hits — an agent typing "seller listing quota limit"
// wants the files covering most of those words, not that exact string. Matches
// any term; the ranker then sorts by how many distinct terms each file covers.
func planMulti(in PlanInput, tms []string) Plan {
	parts := make([]string, len(tms))
	for i, t := range tms {
		parts[i] = escape(t)
	}
	args := []string{"-n", "--no-heading", "-i"}
	if in.Hidden {
		args = append(args, "--hidden")
	}
	if in.Lang != "" {
		args = append(args, "-t", in.Lang)
	}
	for _, g := range in.Globs {
		args = append(args, "-g", g)
	}
	args = append(args, "-e", "("+strings.Join(parts, "|")+")")
	args = append(args, in.Paths...)
	return Plan{
		RgArgs:      args,
		Passthrough: false,
		Tokens:      tms,
		NTerms:      len(tms),
		DetectDefs:  false,
	}
}
