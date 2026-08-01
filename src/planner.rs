//! Decides how a query is executed: literal, regex, or symbol — and whether
//! scout should get out of the way entirely (passthrough).

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Literal,
    Regex,
    Symbol,
}

#[derive(Debug)]
pub struct Plan {
    /// Verbatim `rg` args, without the `--json` flag.
    pub rg_args: Vec<String>,
    /// True when the query has no literal substring of >= 3 chars. A trigram
    /// index is 28x slower than rg on this class, and ranking a scan with no
    /// anchor is guesswork — so we hand the query straight to rg.
    pub passthrough: bool,
    /// Identifier-ish tokens of the query, used by the ranker to detect
    /// definition shapes. Empty for pure-regex queries.
    pub tokens: Vec<String>,
    /// Number of whitespace-separated words in the query. Coverage scoring only
    /// applies when this is > 1.
    pub n_terms: usize,
    /// Whether definition detection is meaningful. A natural-language query
    /// names no identifier, so nothing in it can be "defined" — and French
    /// prose like `type de vendeur` trips the `type` keyword.
    pub detect_defs: bool,
}

const META: &[char] = &[
    '\\', '^', '$', '.', '|', '?', '*', '+', '(', ')', '[', ']', '{', '}',
];

fn has_meta(q: &str) -> bool {
    q.chars().any(|c| META.contains(&c))
}

/// Longest run of characters that must appear verbatim in any match.
/// Deliberately conservative: metacharacters end the current run, and the
/// contents of `[...]` classes and `{...}` quantifiers count for nothing —
/// `a-z` inside a class is not a literal anyone can search for.
fn longest_literal(q: &str) -> usize {
    let mut best = 0usize;
    let mut cur = 0usize;
    let mut prev_escape = false;
    let mut depth_class = 0u32;
    let mut depth_brace = 0u32;
    for c in q.chars() {
        if prev_escape {
            prev_escape = false;
            cur = 0;
            continue;
        }
        match c {
            '\\' => {
                prev_escape = true;
                cur = 0;
            }
            '[' => {
                depth_class += 1;
                cur = 0;
            }
            ']' => {
                depth_class = depth_class.saturating_sub(1);
                cur = 0;
            }
            '{' => {
                depth_brace += 1;
                cur = 0;
            }
            '}' => {
                depth_brace = depth_brace.saturating_sub(1);
                cur = 0;
            }
            _ if depth_class > 0 || depth_brace > 0 => cur = 0,
            _ if META.contains(&c) => cur = 0,
            _ => {
                cur += 1;
                best = best.max(cur);
            }
        }
    }
    best
}

/// Identifier-ish fragments, split on camelCase and non-alphanumerics.
/// `useEditListingForm` -> ["useeditlistingform", "use", "edit", "listing", "form"]
pub fn tokenize(q: &str) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    let mut word = String::new();
    for c in q.chars() {
        if c.is_alphanumeric() || c == '_' {
            word.push(c);
        } else if !word.is_empty() {
            out.push(std::mem::take(&mut word));
        }
    }
    if !word.is_empty() {
        out.push(word);
    }

    let mut all: Vec<String> = Vec::new();
    for w in &out {
        let lower = w.to_lowercase();
        if lower.len() >= 2 && !all.contains(&lower) {
            all.push(lower);
        }
        let mut part = String::new();
        for c in w.chars() {
            if c.is_uppercase() && !part.is_empty() {
                if part.len() >= 3 {
                    let p = part.to_lowercase();
                    if !all.contains(&p) {
                        all.push(p);
                    }
                }
                part.clear();
            }
            part.push(c);
        }
        if part.len() >= 3 {
            let p = part.to_lowercase();
            if !all.contains(&p) {
                all.push(p);
            }
        }
    }
    all
}

pub struct PlanInput<'a> {
    pub query: &'a str,
    pub kind: Option<Kind>,
    pub paths: &'a [String],
    pub globs: &'a [String],
    pub lang: Option<&'a str>,
    pub ignore_case: bool,
    pub hidden: bool,
}

pub fn plan(input: PlanInput<'_>) -> Plan {
    let kind = input.kind.unwrap_or_else(|| {
        if has_meta(input.query) {
            Kind::Regex
        } else {
            Kind::Literal
        }
    });

    let mut args: Vec<String> = vec!["-n".into(), "--no-heading".into()];

    match kind {
        // -F keeps recall byte-identical to a plain `rg -n <query>`, which is
        // what the bench compares against. No smart-case, no word boundary.
        Kind::Literal => args.push("-F".into()),
        Kind::Symbol => {
            args.push("-F".into());
            args.push("-w".into());
        }
        Kind::Regex => {}
    }
    if input.ignore_case {
        args.push("-i".into());
    }
    if input.hidden {
        args.push("--hidden".into());
    }
    if let Some(l) = input.lang {
        args.push("-t".into());
        args.push(l.to_string());
    }
    for g in input.globs {
        args.push("-g".into());
        args.push(g.clone());
    }
    args.push("-e".into());
    args.push(input.query.to_string());
    for p in input.paths {
        args.push(p.clone());
    }

    let passthrough = kind == Kind::Regex && longest_literal(input.query) < 3;
    let tokens = if kind == Kind::Regex {
        Vec::new()
    } else {
        tokenize(input.query)
    };

    Plan { rg_args: args, passthrough, tokens, n_terms: terms(input.query).len(), detect_defs: true }
}

/// Whitespace-separated words of at least 3 characters.
pub fn terms(q: &str) -> Vec<String> {
    q.split_whitespace()
        .map(|w| w.trim_matches(|c: char| !c.is_alphanumeric() && c != '_').to_lowercase())
        .filter(|w| w.len() >= 3)
        .collect()
}

fn escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if META.contains(&c) {
            out.push('\\');
        }
        out.push(c);
    }
    out
}

/// Fallback for natural-language queries. `scout search "quota limite annonces
/// vendeur"` as a literal string matches nothing — but an agent typing that
/// wants the files that mention most of those words. Matches any term; the
/// ranker then sorts by how many distinct terms each file covers.
pub fn plan_multi(input: PlanInput<'_>, terms: &[String]) -> Plan {
    let alt = terms.iter().map(|t| escape(t)).collect::<Vec<_>>().join("|");
    let mut args: Vec<String> = vec!["-n".into(), "--no-heading".into(), "-i".into()];
    if input.hidden {
        args.push("--hidden".into());
    }
    if let Some(l) = input.lang {
        args.push("-t".into());
        args.push(l.to_string());
    }
    for g in input.globs {
        args.push("-g".into());
        args.push(g.clone());
    }
    args.push("-e".into());
    args.push(format!("({alt})"));
    for p in input.paths {
        args.push(p.clone());
    }
    Plan { rg_args: args, passthrough: false, tokens: terms.to_vec(), n_terms: terms.len(), detect_defs: false }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn literal_runs() {
        assert_eq!(longest_literal("useState"), 8);
        assert_eq!(longest_literal("a.b.c"), 1);
        assert_eq!(longest_literal("foo|bar"), 3);
        assert_eq!(longest_literal(r"\w+\s*=>"), 2);
        // `a-z` lives inside a character class: it is not a searchable literal.
        assert!(longest_literal("[a-z]{2,}") < 3);
        assert_eq!(longest_literal("[a-z]+Handler"), 7);
    }

    #[test]
    fn passthrough_only_without_anchor() {
        let p = plan(PlanInput {
            query: r"\w+\s*=>",
            kind: None,
            paths: &[],
            globs: &[],
            lang: None,
            ignore_case: false,
            hidden: false,
        });
        assert!(p.passthrough, "regex with no 3-char literal must pass through");

        let p = plan(PlanInput {
            query: r"function\s+\w+",
            kind: None,
            paths: &[],
            globs: &[],
            lang: None,
            ignore_case: false,
            hidden: false,
        });
        assert!(!p.passthrough, "regex anchored on 'function' is rankable");
    }

    #[test]
    fn camel_split() {
        let t = tokenize("useEditListingForm");
        assert!(t.contains(&"useeditlistingform".to_string()));
        assert!(t.contains(&"listing".to_string()));
    }
}
