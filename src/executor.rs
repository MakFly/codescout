//! Runs `rg --json` and folds the stream into per-file records.
//! Memory is O(files), never O(matches): counters stay exact while only a
//! handful of representative lines per file are retained.

use std::io::{BufRead, BufReader};
use std::process::{Command, Stdio};

use crate::ranker;

#[derive(Debug, Clone)]
pub struct Line {
    pub n: u64,
    pub text: String,
    pub score: f32,
    pub is_def: bool,
}

#[derive(Debug)]
pub struct FileHit {
    pub path: String,
    pub matches: u64,
    /// Bit i set when token i of the query was seen anywhere in this file.
    /// Tracked over every match line, so coverage is exact even though only a
    /// few lines are retained.
    pub covered: u32,
    /// Best representative lines, highest score first.
    pub best: Vec<Line>,
    /// Best definition-shaped line seen in this file, if any. Tracked over the
    /// whole stream so a definition can never be lost to the retention cap.
    pub def: Option<Line>,
    pub score: f32,
}

#[derive(Debug, Default)]
pub struct Outcome {
    pub files: Vec<FileHit>,
    pub total_matches: u64,
    pub total_files: u64,
    /// Byte size of the equivalent `rg -n --no-heading` output. This is the
    /// faithful baseline the `never_worse` invariant compares against.
    pub faithful_bytes: u64,
    pub rg_exit: i32,
}

const KEEP_PER_FILE: usize = 3;
const MAX_LINE_CHARS: usize = 600;

fn strip_prefix(p: &str) -> &str {
    p.strip_prefix("./").unwrap_or(p)
}

pub fn run(rg_args: &[String], tokens: &[String], detect_defs: bool) -> std::io::Result<Outcome> {
    let mut cmd = Command::new("rg");
    cmd.arg("--json");
    cmd.args(rg_args);
    cmd.stdout(Stdio::piped());
    cmd.stderr(Stdio::piped());

    let mut child = cmd.spawn()?;
    let stdout = child.stdout.take().expect("piped");
    let reader = BufReader::new(stdout);

    let mut out = Outcome::default();
    let mut cur: Option<FileHit> = None;

    for line in reader.lines() {
        let line = line?;
        let v: serde_json::Value = match serde_json::from_str(&line) {
            Ok(v) => v,
            Err(_) => continue,
        };
        let ty = v.get("type").and_then(|t| t.as_str()).unwrap_or("");
        let data = match v.get("data") {
            Some(d) => d,
            None => continue,
        };

        match ty {
            "begin" => {
                let path = data
                    .get("path")
                    .and_then(|p| p.get("text"))
                    .and_then(|t| t.as_str())
                    .unwrap_or("")
                    .to_string();
                cur = Some(FileHit {
                    path: strip_prefix(&path).to_string(),
                    matches: 0,
                    covered: 0,
                    best: Vec::new(),
                    def: None,
                    score: 0.0,
                });
            }
            "match" => {
                let f = match cur.as_mut() {
                    Some(f) => f,
                    None => continue,
                };
                // Non-UTF8 lines arrive as `bytes` (base64); we count them but
                // never try to display them.
                let text = match data.get("lines").and_then(|l| l.get("text")).and_then(|t| t.as_str()) {
                    Some(t) => t,
                    None => {
                        f.matches += 1;
                        out.total_matches += 1;
                        continue;
                    }
                };
                let n = data.get("line_number").and_then(|x| x.as_u64()).unwrap_or(0);

                f.matches += 1;
                out.total_matches += 1;
                out.faithful_bytes += (f.path.len() + 1 + digits(n) + 1 + text.len()) as u64;

                let trimmed = text.trim_end_matches(['\n', '\r']).trim();
                if trimmed.is_empty() {
                    continue;
                }
                let mut display: String = trimmed.chars().take(MAX_LINE_CHARS).collect();
                if display.chars().count() < trimmed.chars().count() {
                    display.push('…');
                }

                let lower = display.to_lowercase();
                for (i, t) in tokens.iter().take(32).enumerate() {
                    if lower.contains(t.as_str()) {
                        f.covered |= 1 << i;
                    }
                }

                let (score, mut is_def) = ranker::line_score(&display, tokens);
                is_def &= detect_defs;
                let l = Line { n, text: display, score, is_def };

                if is_def && f.def.as_ref().map_or(true, |d| l.score > d.score) {
                    f.def = Some(l.clone());
                }
                if f.best.len() < KEEP_PER_FILE {
                    f.best.push(l);
                    f.best.sort_by(|a, b| b.score.total_cmp(&a.score));
                } else if let Some(worst) = f.best.last() {
                    if l.score > worst.score {
                        f.best.pop();
                        f.best.push(l);
                        f.best.sort_by(|a, b| b.score.total_cmp(&a.score));
                    }
                }
            }
            "end" => {
                if let Some(f) = cur.take() {
                    if f.matches > 0 {
                        out.total_files += 1;
                        out.files.push(f);
                    }
                }
            }
            _ => {}
        }
    }

    let status = child.wait()?;
    out.rg_exit = status.code().unwrap_or(2);
    if out.rg_exit == 2 {
        if let Some(mut err) = child.stderr.take() {
            let mut buf = String::new();
            use std::io::Read;
            let _ = err.read_to_string(&mut buf);
            if !buf.trim().is_empty() {
                eprint!("{buf}");
            }
        }
    }
    Ok(out)
}

/// Streams `rg` verbatim to stdout, so output is byte-identical to plain rg.
/// Used for passthrough and for the `never_worse` fallback.
///
/// One deviation, and it is deliberate: the stream stops at `cap` bytes. An
/// unanchored regex like `[a-z]{2,}` produces 18 MB on a mid-size repo, and
/// handing that to an agent is worse than any ranking mistake. The cut is
/// stated on stderr with the exact command to get the rest.
pub fn passthrough(rg_args: &[String], cap: usize) -> std::io::Result<i32> {
    use std::io::Write;

    let mut child = Command::new("rg")
        .args(rg_args)
        .stdout(Stdio::piped())
        .spawn()?;
    let mut reader = BufReader::new(child.stdout.take().expect("piped"));
    let stdout = std::io::stdout();
    let mut out = stdout.lock();

    let mut written = 0usize;
    let mut truncated = false;
    let mut buf = String::new();
    loop {
        buf.clear();
        match reader.read_line(&mut buf) {
            Ok(0) => break,
            Ok(_) => {
                if written + buf.len() > cap {
                    truncated = true;
                    break;
                }
                if out.write_all(buf.as_bytes()).is_err() {
                    break;
                }
                written += buf.len();
            }
            Err(_) => break,
        }
    }
    let _ = out.flush();

    if truncated {
        let _ = child.kill();
        eprintln!(
            "\nscout: output cut at {written} bytes — this query has no literal anchor, so \
             scout cannot rank it.\nNarrow it, or run rg directly to see everything."
        );
    }
    let status = child.wait()?;
    Ok(if truncated { 0 } else { status.code().unwrap_or(2) })
}

fn digits(mut n: u64) -> usize {
    if n == 0 {
        return 1;
    }
    let mut d = 0;
    while n > 0 {
        d += 1;
        n /= 10;
    }
    d
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn digit_count() {
        assert_eq!(digits(0), 1);
        assert_eq!(digits(9), 1);
        assert_eq!(digits(10), 2);
        assert_eq!(digits(1234), 4);
    }

    #[test]
    fn dot_slash_stripped() {
        assert_eq!(strip_prefix("./src/a.ts"), "src/a.ts");
        assert_eq!(strip_prefix("src/a.ts"), "src/a.ts");
    }
}
