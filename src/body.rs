//! Point A: attach the source of the top definition to the search result.
//!
//! Measured motivation: in the A/B, every `[def]` hit was followed by a `Read`
//! on that exact file around that exact line. That second round-trip is pure
//! waste — scout already knows the line. Bytes are linear, turns are quadratic
//! (each turn re-bills the whole conversation), so trading bytes for a turn is
//! structurally favourable *if* the body actually answers the question.
//!
//! Nothing here is synthesised: every emitted line is a verbatim slice of the
//! file, prefixed with its true line number, and any cut is stated with the
//! command that reveals the rest.

use std::fs;

pub struct Body {
    pub path: String,
    pub from: u64,
    pub to: u64,
    pub lines: Vec<(u64, String)>,
    /// Stopped on `max_lines` rather than on the end of the construct.
    pub truncated: bool,
    pub file_lines: u64,
}

fn indent_of(s: &str) -> usize {
    let mut n = 0;
    for c in s.chars() {
        match c {
            ' ' => n += 1,
            '\t' => n += 4,
            _ => break,
        }
    }
    n
}

/// A line that only opens a block (`{` on its own, PHP/C brace style) is part
/// of the construct even though it sits at the definition's indent level.
fn is_pure_opener(t: &str) -> bool {
    !t.is_empty() && t.chars().all(|c| matches!(c, '{' | '(' | '[' | ':'))
}

/// A line sitting at the definition's indent level that nonetheless *opens* the
/// real block: the tail of a multi-line signature (`): Promise<T> {`), an `else`
/// arm, a Python `def f(\n  a,\n):`. Without this, a definition whose parameter
/// list spans several lines yields a body consisting of its signature only —
/// measured on a real `export async function f(\n  id: number,\n): Promise<R> {`,
/// which returned its 3 signature lines and no code at all.
fn continues_construct(t: &str) -> bool {
    let t = t.trim_end();
    t.ends_with('{') || t.ends_with('(') || t.ends_with('[') || t.ends_with("=>") || t.ends_with(':')
}

fn is_closer(t: &str) -> bool {
    t.starts_with('}')
        || t.starts_with(')')
        || t.starts_with(']')
        || t == "end"
        || t == "fi"
        || t == "done"
        || t == "esac"
}

const MAX_BODY_LINE_CHARS: usize = 300;

/// Slices the construct starting at `line` (1-based). Returns `None` when the
/// file is unreadable, the line is out of range, or the construct is a
/// one-liner — in that last case the definition line is already shown in the
/// index and a body block would add nothing.
pub fn extract(path: &str, line: u64, max_lines: usize) -> Option<Body> {
    if max_lines == 0 || line == 0 {
        return None;
    }
    let text = fs::read_to_string(path).ok()?;
    let all: Vec<&str> = text.lines().collect();
    let file_lines = all.len() as u64;
    let start = (line - 1) as usize;
    if start >= all.len() {
        return None;
    }

    let base = indent_of(all[start]);
    let mut out: Vec<(u64, String)> = Vec::new();
    let mut truncated = false;

    for (off, raw) in all[start..].iter().enumerate() {
        if out.len() >= max_lines {
            truncated = true;
            break;
        }
        let trimmed = raw.trim();
        if off > 0
            && !trimmed.is_empty()
            && indent_of(raw) <= base
            && !is_pure_opener(trimmed)
            && !continues_construct(trimmed)
        {
            // Back at (or above) the definition's level: the construct is over.
            // A closing token belongs to it, anything else does not.
            if is_closer(trimmed) {
                out.push((line + off as u64, clip(raw)));
            }
            break;
        }
        out.push((line + off as u64, clip(raw)));
    }

    // Trailing blank lines carry nothing.
    while out.last().is_some_and(|(_, t)| t.trim().is_empty()) {
        out.pop();
    }
    if out.len() < 2 {
        return None;
    }

    let from = out.first()?.0;
    let to = out.last()?.0;
    Some(Body { path: path.to_string(), from, to, lines: out, truncated, file_lines })
}

fn clip(s: &str) -> String {
    if s.chars().count() <= MAX_BODY_LINE_CHARS {
        return s.to_string();
    }
    let mut out: String = s.chars().take(MAX_BODY_LINE_CHARS).collect();
    out.push('…');
    out
}

pub fn render(b: &Body) -> String {
    let mut s = String::from("\n");
    s.push_str(&format!("── source {}:{}-{} ", b.path, b.from, b.to));
    s.push_str(&"─".repeat(20));
    s.push('\n');
    let w = b.to.to_string().len();
    for (n, t) in &b.lines {
        s.push_str(&format!("{:>w$} │ {}\n", n, t, w = w));
    }
    if b.truncated {
        s.push_str(&format!(
            "… cut at {} lines (file: {} lines) — rest: sed -n '{},{}p' {}\n",
            b.lines.len(),
            b.file_lines,
            b.to + 1,
            (b.to + 40).min(b.file_lines),
            b.path
        ));
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn tmp(name: &str, content: &str) -> String {
        let p = std::env::temp_dir().join(format!("scout-body-{name}"));
        let mut f = fs::File::create(&p).unwrap();
        f.write_all(content.as_bytes()).unwrap();
        p.to_string_lossy().to_string()
    }

    #[test]
    fn stops_at_dedent_and_keeps_the_closer() {
        let p = tmp("dedent.ts", "function a() {\n  return 1;\n}\nfunction b() {\n  return 2;\n}\n");
        let b = extract(&p, 1, 30).unwrap();
        assert_eq!(b.to, 3, "must stop after a's closing brace, not run into b");
        assert!(!b.truncated);
        assert_eq!(b.lines.last().unwrap().1.trim(), "}");
    }

    #[test]
    fn brace_on_its_own_line_is_not_a_terminator() {
        let p = tmp("php.php", "class C\n{\n    public $x = 1;\n}\nclass D\n{\n}\n");
        let b = extract(&p, 1, 30).unwrap();
        assert_eq!(b.to, 4, "PHP/Allman brace style must not end the construct early");
    }

    #[test]
    fn multiline_signature_does_not_end_the_body() {
        let p = tmp("sig.ts", concat!(
            "export async function act(\n",
            "  id: number,\n",
            "): Promise<Res> {\n",
            "  const a = 1;\n",
            "  return a;\n",
            "}\n",
            "export function other() {}\n",
        ));
        let b = extract(&p, 1, 30).unwrap();
        assert_eq!(b.to, 6, "the `): Promise<Res> {{` line opens the block, it does not close it");
        assert!(b.lines.iter().any(|(_, t)| t.contains("return a")), "the code must be there");
    }

    #[test]
    fn one_liner_yields_no_body() {
        let p = tmp("const.ts", "const THRESHOLD = 0.45;\nconst OTHER = 1;\n");
        assert!(extract(&p, 1, 30).is_none(), "the index line already shows it");
    }

    #[test]
    fn truncation_is_flagged_not_hidden() {
        let mut src = String::from("function big() {\n");
        for i in 0..100 {
            src.push_str(&format!("  const x{i} = {i};\n"));
        }
        src.push_str("}\n");
        let p = tmp("big.ts", &src);
        let b = extract(&p, 1, 10).unwrap();
        assert!(b.truncated);
        assert_eq!(b.lines.len(), 10);
        assert!(render(&b).contains("sed -n"), "the cut states how to see the rest");
    }

    #[test]
    fn every_emitted_line_is_verbatim() {
        let src = "function f() {\n  const a = 1;\n  return a;\n}\n";
        let p = tmp("verbatim.ts", src);
        let b = extract(&p, 1, 30).unwrap();
        let file: Vec<&str> = src.lines().collect();
        for (n, t) in &b.lines {
            assert_eq!(t, file[(*n - 1) as usize], "no line is ever rewritten");
        }
    }

    #[test]
    fn missing_file_is_not_a_crash() {
        assert!(extract("/nonexistent/nope.ts", 1, 30).is_none());
    }
}
