//! Fills a token budget with the highest-ranked evidence, and states plainly
//! what it did not show. Counters are always the true ones; nothing is
//! silently dropped and no synthetic marker is ever injected into source text.

use std::collections::HashMap;

use crate::executor::{FileHit, Outcome};
use crate::ranker::Explain;

const DISPLAY_MAX: usize = 150;
/// Code sits around 3.3 characters per token; `bytes / 4` (what rtk uses)
/// overestimates savings on source lines.
const CHARS_PER_TOKEN: f32 = 3.3;

pub fn est_tokens(s: &str) -> usize {
    (s.chars().count() as f32 / CHARS_PER_TOKEN).round() as usize
}

fn clip(s: &str) -> String {
    if s.chars().count() <= DISPLAY_MAX {
        return s.to_string();
    }
    let mut out: String = s.chars().take(DISPLAY_MAX).collect();
    out.push('…');
    out
}

fn entry(f: &FileHit, explains: Option<&HashMap<String, Explain>>) -> String {
    let l = f.def.as_ref().or_else(|| f.best.first());
    let mut s = String::new();
    match l {
        Some(l) => {
            s.push_str(&format!("{}:{}", f.path, l.n));
            if f.matches > 1 {
                s.push_str(&format!("  ({}x)", f.matches));
            }
            if l.is_def {
                s.push_str("  [def]");
            }
            if let Some(e) = explains.and_then(|m| m.get(&f.path)) {
                s.push_str(&format!(
                    "  {{score={:.2} line={:.2} cov={:.2} dens={:.2} path={:.2} name={:.2} dedup={:.2}}}",
                    f.score, e.line, e.coverage, e.density, e.path_prior, e.name_bonus, e.dedup
                ));
            }
            s.push('\n');
            s.push_str("    ");
            s.push_str(&clip(&l.text));
            s.push('\n');
        }
        None => {
            s.push_str(&format!("{}  ({}x)\n", f.path, f.matches));
        }
    }
    s
}

pub struct RenderOpts<'a> {
    pub query: &'a str,
    /// The literal query matched nothing and we fell back to term coverage.
    pub fallback: bool,
    pub budget_tokens: usize,
    pub page: usize,
    pub repro: String,
    pub elapsed_ms: u128,
    pub explains: Option<&'a HashMap<String, Explain>>,
}

pub fn render(out: &Outcome, opts: RenderOpts<'_>) -> String {
    if out.total_matches == 0 {
        return format!(
            "query: {}   hits=0 files=0\nrepro: {}\n",
            opts.query, opts.repro
        );
    }

    // Header and footer are part of the budget, so reserve for them.
    let budget_chars = (opts.budget_tokens as f32 * CHARS_PER_TOKEN) as usize;
    let reserve = 260usize;
    let usable = budget_chars.saturating_sub(reserve).max(200);

    // Walk the ranked list page by page; a page is full when the next entry
    // would cross the budget. Deterministic, so `--page N` is stable.
    let mut pages: Vec<(usize, usize)> = Vec::new();
    let mut start = 0usize;
    let mut used = 0usize;
    for (i, f) in out.files.iter().enumerate() {
        let e = entry(f, opts.explains);
        let n = e.chars().count();
        if used + n > usable && i > start {
            pages.push((start, i));
            start = i;
            used = 0;
        }
        used += n;
    }
    pages.push((start, out.files.len()));

    let total_pages = pages.len();
    let page = opts.page.clamp(1, total_pages);
    let (from, to) = pages[page - 1];

    let mut body = String::new();
    for f in &out.files[from..to] {
        body.push_str(&entry(f, opts.explains));
    }

    let shown = to - from;
    let remaining = out.files.len() - to;
    // Definitions are sorted to the front, so any that are off-page are on a
    // later page — the count is stated rather than left implicit.
    let defs_offpage = out.files[..from]
        .iter()
        .chain(out.files[to..].iter())
        .filter(|f| f.def.is_some())
        .count();

    let mut tail = String::from("\n");
    if remaining > 0 {
        tail.push_str(&format!(
            "+{} files not shown — scout search '{}' --page {}   (or --path/--lang/--kind symbol to narrow)\n",
            remaining,
            opts.query,
            page + 1
        ));
    }
    if defs_offpage > 0 {
        tail.push_str(&format!(
            "note: {} file(s) holding a definition are not on this page\n",
            defs_offpage
        ));
    }
    tail.push_str(&format!("repro: {}\n", opts.repro));

    let warn = if opts.fallback {
        "note: no match for the literal string. Fell back to term coverage —\n\
         ranking is unreliable here, prefer an exact identifier or --kind symbol.\n"
    } else {
        ""
    };
    let header = format!(
        "query: {}   hits={} files={} shown={} page={}/{}   ~{}tok  {}ms\n{}\n",
        opts.query,
        out.total_matches,
        out.total_files,
        shown,
        page,
        total_pages,
        est_tokens(&body) + est_tokens(&tail),
        opts.elapsed_ms,
        warn
    );
    header + &body + &tail
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tokens_estimate() {
        assert_eq!(est_tokens(""), 0);
        assert!(est_tokens(&"x".repeat(33)) == 10);
    }

    #[test]
    fn clip_marks_truncation_without_inventing_syntax() {
        let long = "a".repeat(400);
        let c = clip(&long);
        assert!(c.ends_with('…'));
        assert!(!c.contains("//"), "no synthetic comment is ever injected");
    }
}
