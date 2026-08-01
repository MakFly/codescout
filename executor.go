// Runs `rg --json` and folds the stream into per-file records.
// Memory is O(files), never O(matches): counters stay exact while only a
// handful of representative lines per file are retained.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"unicode/utf8"
)

type Line struct {
	N     uint64
	Text  string
	Score float32
	IsDef bool
}

type FileHit struct {
	Path    string
	Matches uint64
	// Bit i set when token i of the query was seen anywhere in this file.
	// Tracked over every match line, so coverage is exact even though only a
	// few lines are retained.
	Covered uint32
	// Best representative lines, highest score first.
	Best []Line
	// Best definition-shaped line seen in this file, if any. Tracked over the
	// whole stream so a definition can never be lost to the retention cap.
	Def   *Line
	Score float32
}

type Outcome struct {
	Files      []*FileHit
	TotalMatch uint64
	TotalFiles uint64
	// Byte size of the equivalent `rg -n --no-heading` output. This is the
	// faithful baseline the never_worse invariant compares against.
	FaithfulBytes uint64
	RgExit        int
}

const (
	keepPerFile  = 3
	maxLineChars = 600
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// takeRunes returns the first n runes of s and whether s was longer.
func takeRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i, count := 0, 0
	for i < len(s) && count < n {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s[:i], true
}

func stripPrefix(p string) string { return strings.TrimPrefix(p, "./") }

func digits(n uint64) int {
	if n == 0 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
}

type rgEvent struct {
	Type string `json:"type"`
	Data *struct {
		Path *struct {
			Text *string `json:"text"`
		} `json:"path"`
		Lines *struct {
			Text *string `json:"text"`
		} `json:"lines"`
		LineNumber *uint64 `json:"line_number"`
	} `json:"data"`
}

func runRg(rgArgs []string, tokens []string, detectDefs bool) (*Outcome, error) {
	args := append([]string{"--json"}, rgArgs...)
	cmd := exec.Command("rg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	out := &Outcome{}
	var cur *FileHit
	// ReadString rather than Scanner: a single minified line can exceed any
	// fixed token cap, and dropping it would corrupt the counters.
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		raw, err := r.ReadString('\n')
		if len(raw) > 0 {
			var ev rgEvent
			if json.Unmarshal([]byte(strings.TrimRight(raw, "\n")), &ev) == nil && ev.Data != nil {
				switch ev.Type {
				case "begin":
					path := ""
					if ev.Data.Path != nil && ev.Data.Path.Text != nil {
						path = *ev.Data.Path.Text
					}
					cur = &FileHit{Path: stripPrefix(path)}
				case "match":
					if cur == nil {
						break
					}
					// Non-UTF8 lines arrive as `bytes` (base64); we count them
					// but never try to display them.
					if ev.Data.Lines == nil || ev.Data.Lines.Text == nil {
						cur.Matches++
						out.TotalMatch++
						break
					}
					text := *ev.Data.Lines.Text
					var n uint64
					if ev.Data.LineNumber != nil {
						n = *ev.Data.LineNumber
					}
					cur.Matches++
					out.TotalMatch++
					out.FaithfulBytes += uint64(len(cur.Path) + 1 + digits(n) + 1 + len(text))

					trimmed := strings.TrimSpace(strings.TrimRight(text, "\n\r"))
					if trimmed == "" {
						break
					}
					display, cut := takeRunes(trimmed, maxLineChars)
					if cut {
						display += "…"
					}
					lower := strings.ToLower(display)
					for i, t := range tokens {
						if i >= 32 {
							break
						}
						if strings.Contains(lower, t) {
							cur.Covered |= 1 << uint(i)
						}
					}
					score, isDef := lineScore(display, tokens)
					isDef = isDef && detectDefs
					l := Line{N: n, Text: display, Score: score, IsDef: isDef}

					if isDef && (cur.Def == nil || l.Score > cur.Def.Score) {
						d := l
						cur.Def = &d
					}
					if len(cur.Best) < keepPerFile {
						cur.Best = append(cur.Best, l)
						sortBest(cur.Best)
					} else if l.Score > cur.Best[len(cur.Best)-1].Score {
						cur.Best[len(cur.Best)-1] = l
						sortBest(cur.Best)
					}
				case "end":
					if cur != nil {
						if cur.Matches > 0 {
							out.TotalFiles++
							out.Files = append(out.Files, cur)
						}
						cur = nil
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			out.RgExit = ee.ExitCode()
		} else {
			return nil, err
		}
	}
	if out.RgExit == 2 && strings.TrimSpace(stderr.String()) != "" {
		os.Stderr.WriteString(stderr.String())
	}
	return out, nil
}

func sortBest(b []Line) {
	sort.SliceStable(b, func(i, j int) bool { return b[i].Score > b[j].Score })
}

// passthrough streams rg verbatim to stdout, so output is byte-identical to
// plain rg. Used for passthrough and for the never_worse fallback.
//
// One deviation, and it is deliberate: the stream stops at `cap` bytes. An
// unanchored regex like `[a-z]{2,}` produces 18 MB on a mid-size repo, and
// handing that to an agent is worse than any ranking mistake. The cut is stated
// on stderr with the exact command to get the rest.
func passthrough(rgArgs []string, capBytes int) (int, error) {
	cmd := exec.Command("rg", rgArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 2, err
	}
	if err := cmd.Start(); err != nil {
		return 2, err
	}
	r := bufio.NewReaderSize(stdout, 1<<20)
	w := bufio.NewWriter(os.Stdout)

	written := 0
	truncated := false
	for {
		buf, err := r.ReadString('\n')
		if len(buf) > 0 {
			if written+len(buf) > capBytes {
				truncated = true
				break
			}
			if _, werr := w.WriteString(buf); werr != nil {
				break
			}
			written += len(buf)
		}
		if err != nil {
			break
		}
	}
	w.Flush()

	if truncated {
		_ = cmd.Process.Kill()
		fmt.Fprintf(os.Stderr,
			"\nscout: output cut at %d bytes — this query has no literal anchor, so "+
				"scout cannot rank it.\nNarrow it, or run rg directly to see everything.\n",
			written)
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 2
		}
	}
	if truncated {
		return 0, nil
	}
	return code, nil
}
