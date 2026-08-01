package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- executor

func TestDigitCount(t *testing.T) {
	for _, c := range []struct {
		in   uint64
		want int
	}{{0, 1}, {9, 1}, {10, 2}, {1234, 4}} {
		if got := digits(c.in); got != c.want {
			t.Errorf("digits(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestDotSlashStripped(t *testing.T) {
	if stripPrefix("./src/a.ts") != "src/a.ts" || stripPrefix("src/a.ts") != "src/a.ts" {
		t.Fatal("./ prefix handling is wrong")
	}
}

// --------------------------------------------------------------- formatter

func TestTokensEstimate(t *testing.T) {
	if estTokens("") != 0 {
		t.Error("empty string is 0 tokens")
	}
	if estTokens(strings.Repeat("x", 33)) != 10 {
		t.Errorf("33 chars should estimate to 10 tokens, got %d", estTokens(strings.Repeat("x", 33)))
	}
}

func TestClipMarksTruncationWithoutInventingSyntax(t *testing.T) {
	c := clip(strings.Repeat("a", 400))
	if !strings.HasSuffix(c, "…") {
		t.Error("a clipped line must be marked")
	}
	if strings.Contains(c, "//") {
		t.Error("no synthetic comment is ever injected")
	}
}

// ----------------------------------------------------------------- planner

func TestLiteralRuns(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"useState", 8}, {"a.b.c", 1}, {"foo|bar", 3},
		{`\w+\s*=>`, 2}, {"[a-z]+Handler", 7},
	} {
		if got := longestLiteral(c.in); got != c.want {
			t.Errorf("longestLiteral(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// `a-z` lives inside a character class: it is not a searchable literal.
	if longestLiteral("[a-z]{2,}") >= 3 {
		t.Error("a character class must not count as a literal anchor")
	}
}

func TestPassthroughOnlyWithoutAnchor(t *testing.T) {
	p := plan(PlanInput{Query: `\w+\s*=>`})
	if !p.Passthrough {
		t.Error("regex with no 3-char literal must pass through")
	}
	p = plan(PlanInput{Query: `function\s+\w+`})
	if p.Passthrough {
		t.Error("regex anchored on 'function' is rankable")
	}
}

func TestCamelSplit(t *testing.T) {
	got := tokenize("useEditListingForm")
	if !contains(got, "useeditlistingform") || !contains(got, "listing") {
		t.Errorf("camelCase split lost a fragment: %v", got)
	}
}

// ------------------------------------------------------------------ ranker

func TestCallIsNotADefinition(t *testing.T) {
	tk := tokenize("useState")
	// Regression: these shapes produced 296 false [def] marks on a single query
	// before the assignment guard.
	for _, line := range []string{
		"const [rows, setRows] = useState<StockRow[]>([]);",
		`  const [inputMode, setInputMode] = React.useState<"plate" | "manual">("plate")`,
		"    const [isMobile, setIsMobile] = React.useState<boolean | undefined>(undefined)",
		"  setCount(useState(0));",
	} {
		if _, d := lineScore(line, tk); d {
			t.Errorf("a useState() call must not be read as a definition: %s", line)
		}
	}
}

func TestDefinitionOfTheQueriedSymbolStillDetected(t *testing.T) {
	// The same shape *is* a definition when the query names the thing being
	// declared rather than the thing being called.
	if _, d := lineScore("  const [rows, setRows] = useState<StockRow[]>([]);", tokenize("setRows")); !d {
		t.Error("setRows is genuinely declared on this line")
	}
}

func TestDefinitionsAcrossLanguages(t *testing.T) {
	for _, c := range [][2]string{
		{"handleSubmit", "export const handleSubmit = async (e: Event) => {"},
		{"handleSubmit", "export default function handleSubmit(e) {"},
		{"UserService", "export class UserService implements IUserService {"},
		{"Handle", "func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {"},
		{"main", "int main(int argc, char **argv) {"},
		{"parse_config", "pub(crate) fn parse_config(path: &Path) -> Result<Config> {"},
		{"get_user", "    def get_user(self, user_id: int) -> User:"},
		{"Repository", "type Repository interface {"},
		{"handleClick", "  handleClick() {"},
		{"Config", "pub struct Config {"},
	} {
		if _, d := lineScore(c[1], tokenize(c[0])); !d {
			t.Errorf("should detect a definition in: %s", c[1])
		}
	}
}

func TestImportsRankBelowUses(t *testing.T) {
	tk := tokenize("useState")
	imp, _ := lineScore("import { useState } from 'react';", tk)
	usage, _ := lineScore("  const [a, setA] = useState(null);", tk)
	if usage <= imp {
		t.Error("an import line must not outrank a real use")
	}
}

func TestProseIsNotADefinitionSource(t *testing.T) {
	// Prose put marketing copy at the top of a natural-language query because
	// `type` is a definition keyword. Multi-term queries now disable definition
	// detection entirely; this test pins the shape that exposed it.
	if _, d := lineScore("<p>La protection légale dépend du type de vendeur :</p>", tokenize("vendeur")); !d {
		t.Error("the backward scan does fire here — which is why planMulti turns it off")
	}
}

func TestJunkPathsAreDemoted(t *testing.T) {
	if pathPrior("node_modules/react/index.js") >= pathPrior("apps/web/src/a.ts") {
		t.Error("vendored code must rank below source")
	}
	if pathPrior("src/a.test.ts") >= pathPrior("src/a.ts") {
		t.Error("tests must rank below the code they test")
	}
}

// -------------------------------------------------------------------- body

func tmpFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStopsAtDedentAndKeepsTheCloser(t *testing.T) {
	p := tmpFile(t, "dedent.ts", "function a() {\n  return 1;\n}\nfunction b() {\n  return 2;\n}\n")
	b := extractBody(p, 1, 30)
	if b == nil || b.To != 3 {
		t.Fatalf("must stop after a's closing brace, not run into b: %+v", b)
	}
	if b.Truncated || strings.TrimSpace(b.Lines[len(b.Lines)-1].Text) != "}" {
		t.Error("the closing brace belongs to the construct")
	}
}

func TestBraceOnItsOwnLineIsNotATerminator(t *testing.T) {
	p := tmpFile(t, "php.php", "class C\n{\n    public $x = 1;\n}\nclass D\n{\n}\n")
	b := extractBody(p, 1, 30)
	if b == nil || b.To != 4 {
		t.Fatalf("PHP/Allman brace style must not end the construct early: %+v", b)
	}
}

func TestMultilineSignatureDoesNotEndTheBody(t *testing.T) {
	p := tmpFile(t, "sig.ts", "export async function act(\n  id: number,\n): Promise<Res> {\n  const a = 1;\n  return a;\n}\nexport function other() {}\n")
	b := extractBody(p, 1, 30)
	if b == nil || b.To != 6 {
		t.Fatalf("`): Promise<Res> {` opens the block, it does not close it: %+v", b)
	}
	found := false
	for _, l := range b.Lines {
		if strings.Contains(l.Text, "return a") {
			found = true
		}
	}
	if !found {
		t.Error("the code must be there, not just the signature")
	}
}

func TestOneLinerYieldsNoBody(t *testing.T) {
	p := tmpFile(t, "const.ts", "const THRESHOLD = 0.45;\nconst OTHER = 1;\n")
	if extractBody(p, 1, 30) != nil {
		t.Error("the index line already shows it")
	}
}

func TestTruncationIsFlaggedNotHidden(t *testing.T) {
	var src strings.Builder
	src.WriteString("function big() {\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&src, "  const x%d = %d;\n", i, i)
	}
	src.WriteString("}\n")
	p := tmpFile(t, "big.ts", src.String())
	b := extractBody(p, 1, 10)
	if b == nil || !b.Truncated || len(b.Lines) != 10 {
		t.Fatalf("a cut must be flagged: %+v", b)
	}
	if !strings.Contains(renderBody(b), "sed -n") {
		t.Error("the cut states how to see the rest")
	}
}

func TestEveryEmittedLineIsVerbatim(t *testing.T) {
	src := "function f() {\n  const a = 1;\n  return a;\n}\n"
	p := tmpFile(t, "verbatim.ts", src)
	b := extractBody(p, 1, 30)
	if b == nil {
		t.Fatal("expected a body")
	}
	file := strings.Split(src, "\n")
	for _, l := range b.Lines {
		if l.Text != file[l.N-1] {
			t.Errorf("line %d was rewritten: %q vs %q", l.N, l.Text, file[l.N-1])
		}
	}
}

func TestMissingFileIsNotACrash(t *testing.T) {
	if extractBody("/nonexistent/nope.ts", 1, 30) != nil {
		t.Error("an unreadable file yields no body, not a panic")
	}
}

// ------------------------------------------------------------ arg parsing

// The benchmark shims append flags AFTER the positionals, which Go's stdlib
// flag package would silently ignore. This pins the behaviour clap gave us.
func TestFlagsAfterPositionalsAreParsed(t *testing.T) {
	a, err := parseSearch([]string{"myQuery", ".", "--body", "30", "--page", "2"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Query != "myQuery" || len(a.Paths) != 1 || a.Paths[0] != "." {
		t.Errorf("positionals mangled: %+v", a)
	}
	if a.Body != 30 || a.Page != 2 {
		t.Errorf("trailing flags ignored: body=%d page=%d", a.Body, a.Page)
	}
}

func TestEqualsFormAndDefaults(t *testing.T) {
	a, err := parseSearch([]string{"--kind=symbol", "q"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindSymbol {
		t.Error("--flag=value form must work")
	}
	if a.Body != 0 {
		t.Error("--body defaults to 0: measured with no resolvable effect")
	}
	if a.BudgetTokens != 1500 || a.Page != 1 || a.PassthroughCap != 100000 {
		t.Errorf("defaults drifted: %+v", a)
	}
}
