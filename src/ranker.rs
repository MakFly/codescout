//! The product. Turns an unordered pile of matches into an ordered one.
//!
//! Two rules are structural, not heuristic:
//!   1. a file holding a definition-shaped match always sorts above files that
//!      do not, whatever its path prior — a definition is never buried;
//!   2. every score component is reproducible and dumpable via `--explain`.

use std::collections::HashMap;

use crate::executor::FileHit;

/// Words that introduce a definition. Kept deliberately small: the backward
/// scan below is what gives precision, not the size of this list.
const DEF_KW: &[&str] = &[
    "function", "fn", "def", "class", "interface", "type", "enum", "struct",
    "impl", "trait", "const", "let", "var", "export", "default", "public",
    "private", "protected", "static", "async", "func", "module", "namespace",
    "abstract", "override", "val", "object", "record", "protocol", "extension",
    "component", "procedure", "method", "macro_rules", "declare", "readonly",
];

/// Characters that break the link between a definition keyword and an
/// identifier. `=` is what stops `const [rows, setRows] = useState(...)` from
/// being read as a definition of `useState`; `[` `]` `,` are deliberately
/// absent so destructuring declarations (`const [rows, setRows] = …` seen from
/// `setRows`) are still recognised.
const BREAKERS: &[char] = &['=', '(', ')', '{', '}', ';', ':', '?', '|', '&', '<', '>'];

fn is_ident_char(c: char) -> bool {
    c.is_alphanumeric() || c == '_' || c == '$'
}

/// Byte offsets of every whole-word occurrence of any token in `line`.
fn token_positions(line: &str, tokens: &[String]) -> Vec<(usize, usize)> {
    let lower = line.to_lowercase();
    let bytes = lower.as_bytes();
    let mut out = Vec::new();
    for t in tokens {
        if t.len() < 2 {
            continue;
        }
        let mut from = 0usize;
        while let Some(rel) = lower[from..].find(t.as_str()) {
            let start = from + rel;
            let end = start + t.len();
            let before_ok = start == 0 || !is_ident_char(bytes[start - 1] as char);
            let after_ok = end >= bytes.len() || !is_ident_char(bytes[end] as char);
            if before_ok && after_ok {
                out.push((start, end));
            }
            from = start + 1;
            if from >= lower.len() {
                break;
            }
        }
    }
    out
}

/// Walks backwards from an identifier looking for a definition keyword,
/// stopping at the first breaker character.
fn backward_def(line: &str, start: usize) -> bool {
    let head = &line[..start];
    let mut word = String::new();
    for c in head.chars().rev() {
        if is_ident_char(c) {
            word.insert(0, c);
            continue;
        }
        if !word.is_empty() {
            if DEF_KW.contains(&word.to_lowercase().as_str()) {
                return true;
            }
            word.clear();
        }
        if BREAKERS.contains(&c) {
            return false;
        }
    }
    !word.is_empty() && DEF_KW.contains(&word.to_lowercase().as_str())
}

fn first_word_is_def_kw(line: &str) -> bool {
    let w: String = line.trim_start().chars().take_while(|c| is_ident_char(*c)).collect();
    DEF_KW.contains(&w.to_lowercase().as_str())
}

fn is_comment(line: &str) -> bool {
    let t = line.trim_start();
    t.starts_with("//") || t.starts_with('#') || t.starts_with("* ") || t.starts_with("/*")
        || t.starts_with("--") || t.starts_with("<!--")
}

fn is_import(line: &str) -> bool {
    let t = line.trim_start();
    t.starts_with("import ") || t.starts_with("from ") || t.starts_with("#include")
        || t.starts_with("use ") || t.starts_with("require(") || t.starts_with("export * ")
        || t.starts_with("export {")
}

/// Scores one match line and says whether it looks like a definition.
pub fn line_score(line: &str, tokens: &[String]) -> (f32, bool) {
    let mut score = 1.0f32;
    let positions = token_positions(line, tokens);
    let whole_word = !positions.is_empty();

    let mut is_def = false;
    for (start, end) in &positions {
        if backward_def(line, *start) {
            is_def = true;
            break;
        }
        let after = line[*end..].trim_start();
        let followed_by_paren = after.starts_with('(') || after.starts_with('<');
        // An assignment before the identifier means it is being *called*, not
        // declared: `const [rows, setRows] = useState(...)` defines rows, not
        // useState. Without this guard every React hook call reads as a
        // definition — measured: 296 false positives on a single query.
        let assigned_to = line[..*start].contains('=');
        if followed_by_paren && !assigned_to {
            if first_word_is_def_kw(line) {
                is_def = true;
                break;
            }
            // C-style / Go / class methods: `int main() {`, `handleClick() {`
            let before = line[..*start].chars().last();
            let call_like = matches!(before, Some('.') | Some('>'));
            if !call_like && line.trim_end().ends_with('{') {
                is_def = true;
                break;
            }
        }
    }

    if is_def {
        score += 3.0;
    }
    if whole_word {
        score += 0.6;
    }
    if line.chars().count() < 120 {
        score += 0.3;
    }
    if is_import(line) {
        score -= 0.9;
    }
    if is_comment(line) {
        score -= 0.6;
    }
    (score, is_def)
}

#[derive(Debug, Default, Clone)]
pub struct Explain {
    pub line: f32,
    pub coverage: f32,
    pub density: f32,
    pub path_prior: f32,
    pub name_bonus: f32,
    pub dedup: f32,
}

fn path_prior(path: &str) -> f32 {
    // Leading slash so the segment patterns below also match at path start.
    let p = format!("/{}", path.to_lowercase());
    let p = p.as_str();
    const JUNK: &[&str] = &[
        "/node_modules/", "/dist/", "/build/", "/.next/", "/out/", "/vendor/",
        "/coverage/", "/target/", ".min.", "/generated/", "/.venv/", "/__pycache__/",
    ];
    const TESTY: &[&str] = &[
        "/test/", "/tests/", "/spec/", "__tests__", "__mocks__", ".test.", ".spec.",
        "/e2e/", "/cypress/", "/fixtures/", "/mocks/", ".stories.", "/storybook/",
    ];
    if JUNK.iter().any(|j| p.contains(j)) {
        return 0.10;
    }
    if TESTY.iter().any(|t| p.contains(t)) {
        return 0.30;
    }
    if p.ends_with(".d.ts") {
        return 0.50;
    }
    if p.contains("/src/") || p.contains("/app/") || p.contains("/lib/") {
        return 1.10;
    }
    1.0
}

fn name_bonus(path: &str, tokens: &[String]) -> f32 {
    let file = path.rsplit('/').next().unwrap_or(path).to_lowercase();
    let stem = file.split('.').next().unwrap_or(&file).replace(['-', '_'], "");
    let dir = path.to_lowercase();
    for t in tokens {
        if t.len() < 3 {
            continue;
        }
        if stem.contains(t.as_str()) {
            return 1.5;
        }
    }
    for t in tokens {
        if t.len() >= 4 && dir.contains(t.as_str()) {
            return 0.7;
        }
    }
    0.0
}

/// Ranks files in place and returns the per-file score breakdown.
pub fn rank(files: &mut [FileHit], tokens: &[String], n_tokens: usize) -> HashMap<String, Explain> {
    // Boilerplate detection: a representative line repeated across many files
    // (a shared import, a generated header) carries little information.
    let mut seen: HashMap<&str, u32> = HashMap::new();
    for f in files.iter() {
        if let Some(l) = f.best.first() {
            *seen.entry(l.text.as_str()).or_insert(0) += 1;
        }
    }
    let repeats: HashMap<String, u32> =
        seen.into_iter().map(|(k, v)| (k.to_string(), v)).collect();

    let mut explains = HashMap::new();
    for f in files.iter_mut() {
        let line = f
            .def
            .as_ref()
            .map(|l| l.score)
            .unwrap_or_else(|| f.best.first().map(|l| l.score).unwrap_or(0.0));
        // Coverage dominates on multi-term queries: a file mentioning four of
        // the five words asked about is what the agent wanted, even if each
        // word appears once. On a single-token query this term is constant and
        // changes nothing.
        let multi = n_tokens > 1;
        let coverage = if multi {
            3.0 * (f.covered.count_ones() as f32 / n_tokens as f32)
        } else {
            0.0
        };
        // On a multi-term query raw match count is not relevance: an SEO page
        // saying "vendeur" 469 times outranked the actual quota module. What
        // discriminates there is the filename, not the volume.
        let density = if multi {
            0.0
        } else {
            (0.35 * (1.0 + f.matches as f32).ln()).min(1.2)
        };
        let prior = path_prior(&f.path);
        // Filename match is the strongest signal available when the query is
        // prose, so it carries more weight there.
        let name = name_bonus(&f.path, tokens) * if multi { 2.0 } else { 1.0 };
        let dedup = match f.best.first() {
            Some(l) if !l.is_def && repeats.get(&l.text).copied().unwrap_or(0) > 3 => 0.6,
            _ => 1.0,
        };
        f.score = (line + coverage + density + name) * prior * dedup;
        explains.insert(
            f.path.clone(),
            Explain { line, coverage, density, path_prior: prior, name_bonus: name, dedup },
        );
    }

    // Hard key first: a file holding a definition never sorts below one that
    // does not. Path priors only order files within each group.
    files.sort_by(|a, b| {
        b.def
            .is_some()
            .cmp(&a.def.is_some())
            .then_with(|| b.score.total_cmp(&a.score))
            .then_with(|| a.path.cmp(&b.path))
    });
    explains
}

#[cfg(test)]
mod tests {
    use super::*;

    fn toks(q: &str) -> Vec<String> {
        crate::planner::tokenize(q)
    }

    #[test]
    fn call_is_not_a_definition() {
        let t = toks("useState");
        // Regression: these three shapes produced 296 false `[def]` marks on a
        // single query before the assignment guard.
        for line in [
            "const [rows, setRows] = useState<StockRow[]>([]);",
            "  const [inputMode, setInputMode] = React.useState<\"plate\" | \"manual\">(\"plate\")",
            "    const [isMobile, setIsMobile] = React.useState<boolean | undefined>(undefined)",
            "  setCount(useState(0));",
        ] {
            let (_, d) = line_score(line, &t);
            assert!(!d, "a useState() call must not be read as a definition: {line}");
        }
    }

    #[test]
    fn definition_of_the_queried_symbol_still_detected() {
        // The same shape *is* a definition when the query names the thing
        // being declared rather than the thing being called.
        let (_, d) = line_score(
            "  const [rows, setRows] = useState<StockRow[]>([]);",
            &toks("setRows"),
        );
        assert!(d, "setRows is genuinely declared on this line");
    }

    #[test]
    fn definitions_across_languages() {
        let cases = [
            ("handleSubmit", "export const handleSubmit = async (e: Event) => {"),
            ("handleSubmit", "export default function handleSubmit(e) {"),
            ("UserService", "export class UserService implements IUserService {"),
            ("Handle", "func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {"),
            ("main", "int main(int argc, char **argv) {"),
            ("parse_config", "pub(crate) fn parse_config(path: &Path) -> Result<Config> {"),
            ("get_user", "    def get_user(self, user_id: int) -> User:"),
            ("Repository", "type Repository interface {"),
            ("handleClick", "  handleClick() {"),
            ("Config", "pub struct Config {"),
        ];
        for (q, line) in cases {
            let (_, d) = line_score(line, &toks(q));
            assert!(d, "should detect a definition in: {line}");
        }
    }

    #[test]
    fn imports_rank_below_uses() {
        let t = toks("useState");
        let (imp, _) = line_score("import { useState } from 'react';", &t);
        let (usage, _) = line_score("  const [a, setA] = useState(null);", &t);
        assert!(usage > imp, "an import line must not outrank a real use");
    }

    #[test]
    fn french_prose_is_not_a_definition_source() {
        // `type de vendeur` put SEO copy at the top of a natural-language
        // query because `type` is a definition keyword. Multi-term queries now
        // disable definition detection entirely; this test pins the shape that
        // exposed it.
        let (_, d) = line_score("<p>La protection légale dépend du type de vendeur :</p>", &toks("vendeur"));
        assert!(d, "the backward scan does fire here — which is why plan_multi turns it off");
    }

    #[test]
    fn junk_paths_are_demoted() {
        assert!(path_prior("node_modules/react/index.js") < path_prior("apps/web/src/a.ts"));
        assert!(path_prior("src/a.test.ts") < path_prior("src/a.ts"));
    }
}
