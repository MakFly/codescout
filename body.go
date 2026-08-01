// Attaches the source of the top definition to a search result.
//
// Off by default (--body 0). The idea is sound on paper — bytes cost linearly,
// turns cost quadratically, so removing the follow-up Read should pay — but
// across 296 real agent runs it moved input tokens by no resolvable amount on
// either harness. It stayed opt-in rather than shipping on a hunch. See
// BENCHMARKS.md.
//
// Nothing here is synthesised: every emitted line is a verbatim slice of the
// file, prefixed with its true line number, and any cut is stated with the
// command that reveals the rest.

package main

import (
	"fmt"
	"os"
	"strings"
)

type Body struct {
	Path      string
	From, To  uint64
	Lines     []bodyLine
	Truncated bool // stopped on maxLines rather than at the end of the construct
	FileLines uint64
}

type bodyLine struct {
	N    uint64
	Text string
}

func indentOf(s string) int {
	n := 0
	for _, c := range s {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// A line that only opens a block (`{` on its own, PHP/C brace style) is part of
// the construct even though it sits at the definition's indent level.
func isPureOpener(t string) bool {
	if t == "" {
		return false
	}
	return strings.Trim(t, "{([:") == ""
}

// A line sitting at the definition's indent level that nonetheless *opens* the
// real block: the tail of a multi-line signature (`): Promise<T> {`), an `else`
// arm, a Python `def f(\n  a,\n):`. Without this, a definition whose parameter
// list spans several lines yields a body consisting of its signature only —
// measured on a real `export async function f(\n  id: number,\n): Promise<R> {`,
// which returned its 3 signature lines and no code at all.
func continuesConstruct(t string) bool {
	t = strings.TrimRight(t, " \t")
	return strings.HasSuffix(t, "{") || strings.HasSuffix(t, "(") ||
		strings.HasSuffix(t, "[") || strings.HasSuffix(t, "=>") ||
		strings.HasSuffix(t, ":")
}

func isCloser(t string) bool {
	return strings.HasPrefix(t, "}") || strings.HasPrefix(t, ")") ||
		strings.HasPrefix(t, "]") || t == "end" || t == "fi" ||
		t == "done" || t == "esac"
}

const maxBodyLineChars = 300

func clipBody(s string) string {
	out, cut := takeRunes(s, maxBodyLineChars)
	if cut {
		return out + "…"
	}
	return out
}

// extractBody slices the construct starting at `line` (1-based). Returns nil
// when the file is unreadable, the line is out of range, or the construct is a
// one-liner — in that last case the definition line is already shown in the
// index and a body block would add nothing.
func extractBody(path string, line uint64, maxLines int) *Body {
	if maxLines == 0 || line == 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	all := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for i := range all {
		// Match Rust's `str::lines()`, which strips a trailing \r on CRLF files.
		all[i] = strings.TrimSuffix(all[i], "\r")
	}
	fileLines := uint64(len(all))
	start := int(line - 1)
	if start >= len(all) {
		return nil
	}

	base := indentOf(all[start])
	var out []bodyLine
	truncated := false

	for off, raw := range all[start:] {
		if len(out) >= maxLines {
			truncated = true
			break
		}
		trimmed := strings.TrimSpace(raw)
		if off > 0 && trimmed != "" && indentOf(raw) <= base &&
			!isPureOpener(trimmed) && !continuesConstruct(trimmed) {
			// Back at (or above) the definition's level: the construct is over.
			// A closing token belongs to it, anything else does not.
			if isCloser(trimmed) {
				out = append(out, bodyLine{line + uint64(off), clipBody(raw)})
			}
			break
		}
		out = append(out, bodyLine{line + uint64(off), clipBody(raw)})
	}

	// Trailing blank lines carry nothing.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1].Text) == "" {
		out = out[:len(out)-1]
	}
	if len(out) < 2 {
		return nil
	}
	return &Body{
		Path: path, From: out[0].N, To: out[len(out)-1].N,
		Lines: out, Truncated: truncated, FileLines: fileLines,
	}
}

func renderBody(b *Body) string {
	var s strings.Builder
	s.WriteString("\n")
	fmt.Fprintf(&s, "── source %s:%d-%d %s\n", b.Path, b.From, b.To, strings.Repeat("─", 20))
	w := len(fmt.Sprintf("%d", b.To))
	for _, l := range b.Lines {
		fmt.Fprintf(&s, "%*d │ %s\n", w, l.N, l.Text)
	}
	if b.Truncated {
		rest := b.To + 40
		if rest > b.FileLines {
			rest = b.FileLines
		}
		fmt.Fprintf(&s, "… cut at %d lines (file: %d lines) — rest: sed -n '%d,%dp' %s\n",
			len(b.Lines), b.FileLines, b.To+1, rest, b.Path)
	}
	return s.String()
}
