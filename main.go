// scout — code search for LLM agents.
//
// One binary, no index, no daemon, no state on disk. It runs `rg`, ranks what
// comes back, and spends a token budget on the best evidence instead of
// truncating alphabetically.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const version = "0.2.0"

type searchArgs struct {
	Query          string
	Paths          []string
	Kind           Kind
	Globs          []string
	Lang           string
	BudgetTokens   int
	Page           int
	JSON           bool
	Explain        bool
	IgnoreCase     bool
	Hidden         bool
	NoGuard        bool
	PassthroughCap int
	Body           int
}

const usage = `scout — ranked, budgeted code search for LLM agents

USAGE:
    scout search <query> [paths...] [options]
    scout doctor

OPTIONS:
    --kind <auto|literal|regex|symbol>  how to interpret the query [auto]
    --path <glob>                       restrict to paths matching a glob (repeatable)
    --lang <ext>                        restrict to an rg file type, e.g. ts, py, rust
    --budget-tokens <n>                 output budget in tokens [1500]
    --page <n>                          page of results to show [1]
    --body <n>                          lines of source to attach for the top
                                        definition; 0 disables it [0]
    --passthrough-cap <bytes>           hard cap on passthrough output [100000]
    --json                              emit JSON instead of text
    --explain                           show the score breakdown for every result
    --no-guard                          disable the never_worse guard (benchmarking only)
    -i, --ignore-case                   case-insensitive search
    --hidden                            include hidden files
    -h, --help                          print this help
    -V, --version                       print version

Use it for identifier and definition lookups. For following a trail or reading a
file, native tools cost fewer turns — see
https://github.com/MakFly/codescout#use-it-for-lookups-not-for-exploration
`

// parseSearch accepts flags interleaved with positionals, the way clap does.
// Go's stdlib flag package stops at the first positional, which would silently
// ignore `scout search q . --body 0` — the exact shape the benchmark shims use.
func parseSearch(argv []string) (searchArgs, error) {
	a := searchArgs{Kind: KindAuto, BudgetTokens: 1500, Page: 1, PassthroughCap: 100000}
	var pos []string

	next := func(i *int, name string) (string, error) {
		if *i+1 >= len(argv) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return argv[*i], nil
	}
	num := func(s, name string) (int, error) {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("%s expects a non-negative integer, got %q", name, s)
		}
		return v, nil
	}

	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		// `--flag=value` is normalised to `--flag value`.
		if strings.HasPrefix(arg, "--") && strings.Contains(arg, "=") {
			k, v, _ := strings.Cut(arg, "=")
			argv = append(argv[:i], append([]string{k, v}, argv[i+1:]...)...)
			arg = k
		}
		var err error
		var v string
		switch arg {
		case "--kind":
			if v, err = next(&i, arg); err != nil {
				return a, err
			}
			switch v {
			case "auto":
				a.Kind = KindAuto
			case "literal":
				a.Kind = KindLiteral
			case "regex":
				a.Kind = KindRegex
			case "symbol":
				a.Kind = KindSymbol
			default:
				return a, fmt.Errorf("--kind expects auto|literal|regex|symbol, got %q", v)
			}
		case "--path":
			if v, err = next(&i, arg); err != nil {
				return a, err
			}
			a.Globs = append(a.Globs, v)
		case "--lang":
			if a.Lang, err = next(&i, arg); err != nil {
				return a, err
			}
		case "--budget-tokens":
			if v, err = next(&i, arg); err != nil {
				return a, err
			}
			if a.BudgetTokens, err = num(v, arg); err != nil {
				return a, err
			}
		case "--page":
			if v, err = next(&i, arg); err != nil {
				return a, err
			}
			if a.Page, err = num(v, arg); err != nil {
				return a, err
			}
		case "--body":
			if v, err = next(&i, arg); err != nil {
				return a, err
			}
			if a.Body, err = num(v, arg); err != nil {
				return a, err
			}
		case "--passthrough-cap":
			if v, err = next(&i, arg); err != nil {
				return a, err
			}
			if a.PassthroughCap, err = num(v, arg); err != nil {
				return a, err
			}
		case "--json":
			a.JSON = true
		case "--explain":
			a.Explain = true
		case "--no-guard":
			a.NoGuard = true
		case "-i", "--ignore-case":
			a.IgnoreCase = true
		case "--hidden":
			a.Hidden = true
		default:
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return a, fmt.Errorf("unknown option %q", arg)
			}
			pos = append(pos, arg)
		}
	}

	if len(pos) == 0 {
		return a, fmt.Errorf("search needs a query")
	}
	a.Query = pos[0]
	a.Paths = pos[1:]
	return a, nil
}

func shellQuote(s string) string {
	ok := s != ""
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("._-/=:@,", c)) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func haveRg() bool {
	return exec.Command("rg", "--version").Run() == nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "-V", "--version":
		fmt.Printf("scout %s\n", version)
	case "doctor":
		doctor()
	case "search":
		a, err := parseSearch(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "scout: %v\n\n", err)
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		search(a)
	default:
		fmt.Fprintf(os.Stderr, "scout: unknown command %q\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func doctor() {
	if out, err := exec.Command("rg", "--version").Output(); err == nil {
		first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		fmt.Printf("rg        OK    %s\n", first)
	} else {
		fmt.Println("rg        MISSING  — scout cannot work without ripgrep")
	}
	if out, err := exec.Command("git", "--version").Output(); err == nil {
		fmt.Printf("git       OK    %s\n", strings.TrimSpace(string(out)))
	} else {
		fmt.Println("git       absent — harmless, scout does not use git in v1")
	}
	fmt.Println("index     none — scout stores nothing on disk")
	fmt.Println("daemon    none — one process per query, < 5 ms startup")
	fmt.Println()
	fmt.Println("Known limit: past roughly 2 GB of source, a cold rg exceeds one")
	fmt.Println("second and scout has nothing to compensate. Wrong tool for that.")
}

func search(a searchArgs) {
	if !haveRg() {
		fmt.Fprintln(os.Stderr, "scout: ripgrep (rg) not found. Run `scout doctor` for details.")
		os.Exit(2)
	}

	paths := a.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}

	in := PlanInput{
		Query: a.Query, Kind: a.Kind, Paths: paths, Globs: a.Globs,
		Lang: a.Lang, IgnoreCase: a.IgnoreCase, Hidden: a.Hidden,
	}
	p := plan(in)

	quoted := make([]string, len(p.RgArgs))
	for i, s := range p.RgArgs {
		quoted[i] = shellQuote(s)
	}
	repro := "rg " + strings.Join(quoted, " ")

	// No literal anchor: ranking would be guesswork, so rg answers directly.
	if p.Passthrough {
		code, err := passthrough(p.RgArgs, a.PassthroughCap)
		if err != nil {
			code = 2
		}
		os.Exit(code)
	}

	t0 := time.Now()
	out, err := runRg(p.RgArgs, p.Tokens, p.DetectDefs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scout: rg failed: %v\n", err)
		os.Exit(2)
	}
	if out.RgExit == 2 {
		os.Exit(2)
	}

	// A multi-word query matched literally is almost always zero hits — an
	// agent typing "seller listing quota limit" wants the files covering most
	// of those words, not that exact string. Retry as an alternation.
	tms := terms(a.Query)
	fallback := false
	if out.TotalMatch == 0 && len(tms) > 1 {
		mi := in
		mi.IgnoreCase = true
		multi := planMulti(mi, tms)
		// Only adopt the fallback if it actually ran: otherwise the header
		// would announce a retry and `repro:` would print a command that did
		// not produce the results shown.
		if o2, err2 := runRg(multi.RgArgs, multi.Tokens, multi.DetectDefs); err2 == nil {
			q2 := make([]string, len(multi.RgArgs))
			for i, s := range multi.RgArgs {
				q2[i] = shellQuote(s)
			}
			repro = "rg " + strings.Join(q2, " ")
			p = multi
			out = o2
			fallback = true
		}
	}

	explains := rank(out.Files, p.Tokens, p.NTerms)
	elapsed := time.Since(t0).Milliseconds()

	if a.JSON {
		fmt.Print(toJSON(out, a.Query, repro, elapsed))
		os.Exit(boolExit(out.TotalMatch > 0))
	}

	var ex map[string]Explain
	if a.Explain {
		ex = explains
	}
	rendered := render(out, RenderOpts{
		Query: a.Query, Fallback: fallback, BudgetTokens: a.BudgetTokens,
		Page: a.Page, Repro: repro, ElapsedMs: elapsed, Explains: ex,
	})

	// Point A: the top-ranked definition carries its source, so the caller does
	// not need a second round-trip to read it. Skipped on the fallback path —
	// there scout declares its own ranking unreliable, and spending 30 lines on
	// a guess it just disowned would contradict that.
	var attached *Body
	if a.Body > 0 && a.Page == 1 && !fallback && len(out.Files) > 0 {
		if d := out.Files[0].Def; d != nil {
			attached = extractBody(out.Files[0].Path, d.N, a.Body)
		}
	}

	// never_worse: if the ranked form is not smaller than the faithful one, the
	// faithful one is what the caller gets.
	//
	// Deliberately evaluated on the INDEX alone. The invariant is "scout is
	// never a worse grep than grep"; the body is not grep output, it is the
	// Read the caller would have issued next. So the guard still decides which
	// *index* to print — ranked or verbatim rg — and the body rides along
	// either way.
	if !a.NoGuard && out.TotalMatch > 0 && uint64(len(rendered)) >= out.FaithfulBytes {
		code, err := passthrough(p.RgArgs, int(^uint(0)>>1))
		if err != nil {
			code = 2
		}
		if attached != nil {
			fmt.Print(renderBody(attached))
		}
		os.Exit(code)
	}

	fmt.Print(rendered)
	if attached != nil {
		fmt.Print(renderBody(attached))
	}
	os.Exit(boolExit(out.TotalMatch > 0))
}

func boolExit(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

func toJSON(out *Outcome, query, repro string, ms int64) string {
	// Maps, not structs, and an Encoder rather than Marshal — two deliberate
	// choices for byte parity with the reference implementation:
	//   - serde_json backs its objects with a BTreeMap, so keys come out
	//     alphabetically; Go sorts map keys the same way, struct fields it does
	//     not (they follow declaration order).
	//   - Go escapes <, > and & as \u003c by default; serde does not.
	// Scores widen to float64 so the shortest round-tripping text matches the
	// f32-through-an-f64-writer the reference emits.
	results := make([]any, 0, len(out.Files))
	for _, f := range out.Files {
		var l *Line
		if f.Def != nil {
			l = f.Def
		} else if len(f.Best) > 0 {
			l = &f.Best[0]
		}
		r := map[string]any{
			"path":    f.Path,
			"matches": f.Matches,
			"score":   float64(f.Score),
			"is_def":  f.Def != nil,
			"line":    nil,
			"text":    nil,
		}
		if l != nil {
			r["line"] = l.N
			r["text"] = l.Text
		}
		results = append(results, r)
	}
	payload := map[string]any{
		"query":          query,
		"hits":           out.TotalMatch,
		"files":          out.TotalFiles,
		"elapsed_ms":     ms,
		"faithful_bytes": out.FaithfulBytes,
		"repro":          repro,
		"results":        results,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return "\n"
	}
	// Encode already terminates with the newline the reference appends.
	return buf.String()
}
