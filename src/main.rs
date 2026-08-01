//! scout — code search for LLM agents.
//!
//! One binary, no index, no daemon, no state on disk. It runs `rg`, ranks what
//! comes back, and spends a token budget on the best evidence instead of
//! truncating alphabetically.

mod body;
mod executor;
mod formatter;
mod planner;
mod ranker;

use std::process::Command;
use std::time::Instant;

use clap::{Parser, Subcommand, ValueEnum};

#[derive(Parser)]
#[command(name = "scout", version, about = "Code search for LLM agents")]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Search the codebase and return ranked, budgeted results.
    Search(SearchArgs),
    /// Report which backends are available and what scout will do with them.
    Doctor,
}

#[derive(Copy, Clone, PartialEq, Eq, ValueEnum)]
enum KindArg {
    Auto,
    Literal,
    Regex,
    Symbol,
}

#[derive(Parser)]
struct SearchArgs {
    /// What to look for.
    query: String,
    /// Paths to search (default: current directory).
    paths: Vec<String>,
    /// How to interpret the query.
    #[arg(long, value_enum, default_value = "auto")]
    kind: KindArg,
    /// Restrict to paths matching a glob (repeatable).
    #[arg(long = "path")]
    globs: Vec<String>,
    /// Restrict to an rg file type, e.g. ts, py, rust.
    #[arg(long)]
    lang: Option<String>,
    /// Output budget in tokens.
    #[arg(long, default_value_t = 1500)]
    budget_tokens: usize,
    /// Page of results to show.
    #[arg(long, default_value_t = 1)]
    page: usize,
    /// Emit JSON instead of text.
    #[arg(long)]
    json: bool,
    /// Show the score breakdown for every result.
    #[arg(long)]
    explain: bool,
    /// Case-insensitive search.
    #[arg(short = 'i', long)]
    ignore_case: bool,
    /// Include hidden files.
    #[arg(long)]
    hidden: bool,
    /// Disable the never_worse guard (benchmarking only).
    #[arg(long)]
    no_guard: bool,
    /// Hard cap on passthrough output, in bytes.
    #[arg(long, default_value_t = 100_000)]
    passthrough_cap: usize,
    /// Lines of source to attach for the top definition. 0 disables it.
    ///
    /// Default 0: measured on 296 agent runs (bench/ab3.py, bench/ab4.py) and
    /// found to move input tokens by no resolvable amount on either harness or
    /// either round. See VERDICT.md, "Point A". Opt in when you know you want
    /// the body; it is not on by default because nothing showed it earns it.
    #[arg(long, default_value_t = 0)]
    body: usize,
}

fn shell_quote(s: &str) -> String {
    if s.chars().all(|c| c.is_alphanumeric() || "._-/=:@,".contains(c)) {
        s.to_string()
    } else {
        format!("'{}'", s.replace('\'', r"'\''"))
    }
}

fn have_rg() -> bool {
    Command::new("rg")
        .arg("--version")
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false)
}

fn main() {
    let cli = Cli::parse();
    match cli.cmd {
        Cmd::Doctor => doctor(),
        Cmd::Search(a) => search(a),
    }
}

fn doctor() {
    let rg = Command::new("rg").arg("--version").output();
    match rg {
        Ok(o) if o.status.success() => {
            let v = String::from_utf8_lossy(&o.stdout);
            println!("rg        OK    {}", v.lines().next().unwrap_or(""));
        }
        _ => println!("rg        MISSING  — scout cannot work without ripgrep"),
    }
    let git = Command::new("git").arg("--version").output();
    match git {
        Ok(o) if o.status.success() => println!(
            "git       OK    {}",
            String::from_utf8_lossy(&o.stdout).trim()
        ),
        _ => println!("git       absent — harmless, scout does not use git in v1"),
    }
    println!("index     none — scout stores nothing on disk");
    println!("daemon    none — one process per query, < 5 ms startup");
    println!();
    println!("Known limit: past roughly 2 GB of source, a cold rg exceeds one");
    println!("second and scout has nothing to compensate. Wrong tool for that.");
}

fn search(a: SearchArgs) {
    if !have_rg() {
        eprintln!("scout: ripgrep (rg) not found. Run `scout doctor` for details.");
        std::process::exit(2);
    }

    let paths = if a.paths.is_empty() {
        vec![".".to_string()]
    } else {
        a.paths.clone()
    };
    let kind = match a.kind {
        KindArg::Auto => None,
        KindArg::Literal => Some(planner::Kind::Literal),
        KindArg::Regex => Some(planner::Kind::Regex),
        KindArg::Symbol => Some(planner::Kind::Symbol),
    };

    let plan = planner::plan(planner::PlanInput {
        query: &a.query,
        kind,
        paths: &paths,
        globs: &a.globs,
        lang: a.lang.as_deref(),
        ignore_case: a.ignore_case,
        hidden: a.hidden,
    });

    let repro = format!(
        "rg {}",
        plan.rg_args.iter().map(|s| shell_quote(s)).collect::<Vec<_>>().join(" ")
    );

    // No literal anchor: ranking would be guesswork, so rg answers directly.
    if plan.passthrough {
        let code = executor::passthrough(&plan.rg_args, a.passthrough_cap).unwrap_or(2);
        std::process::exit(code);
    }

    let t0 = Instant::now();
    let mut plan = plan;
    let mut repro = repro;
    let mut out = match executor::run(&plan.rg_args, &plan.tokens, plan.detect_defs) {
        Ok(o) => o,
        Err(e) => {
            eprintln!("scout: rg failed: {e}");
            std::process::exit(2);
        }
    };
    if out.rg_exit == 2 {
        std::process::exit(2);
    }

    // A multi-word query matched literally is almost always zero hits — an
    // agent typing "quota limite annonces vendeur" wants the files covering
    // most of those words, not that exact string. Retry as an alternation.
    let terms = planner::terms(&a.query);
    let mut fallback = false;
    if out.total_matches == 0 && terms.len() > 1 {
        let multi = planner::plan_multi(
            planner::PlanInput {
                query: &a.query,
                kind,
                paths: &paths,
                globs: &a.globs,
                lang: a.lang.as_deref(),
                ignore_case: true,
                hidden: a.hidden,
            },
            &terms,
        );
        // Only adopt the fallback if it actually ran: otherwise the header
        // would announce a retry and `repro:` would print a command that did
        // not produce the results shown.
        if let Ok(o) = executor::run(&multi.rg_args, &multi.tokens, multi.detect_defs) {
            repro = format!(
                "rg {}",
                multi.rg_args.iter().map(|s| shell_quote(s)).collect::<Vec<_>>().join(" ")
            );
            plan = multi;
            out = o;
            fallback = true;
        }
    }

    let explains = ranker::rank(&mut out.files, &plan.tokens, plan.n_terms);
    let elapsed = t0.elapsed().as_millis();

    if a.json {
        print!("{}", to_json(&out, &a.query, &repro, elapsed));
        std::process::exit(if out.total_matches > 0 { 0 } else { 1 });
    }

    let rendered = formatter::render(
        &out,
        formatter::RenderOpts {
            query: &a.query,
            fallback,
            budget_tokens: a.budget_tokens,
            page: a.page,
            repro: repro.clone(),
            elapsed_ms: elapsed,
            explains: if a.explain { Some(&explains) } else { None },
        },
    );

    // Point A: the top-ranked definition carries its source, so the caller does
    // not need a second round-trip to read it. Skipped on the fallback path —
    // there scout declares its own ranking unreliable, and spending 30 lines on
    // a guess it just disowned would contradict that.
    let attached = if a.body > 0 && a.page == 1 && !fallback {
        out.files
            .first()
            .and_then(|f| f.def.as_ref().map(|d| (f.path.as_str(), d.n)))
            .and_then(|(p, n)| body::extract(p, n, a.body))
    } else {
        None
    };

    // never_worse: if the ranked form is not smaller than the faithful one, the
    // faithful one is what the caller gets. Borrowed from rtk's guard.rs.
    //
    // Deliberately evaluated on the INDEX alone. The invariant is "scout is
    // never a worse grep than grep"; the body is not grep output, it is the
    // Read the caller would have issued next. So the guard still decides which
    // *index* to print — ranked or verbatim rg — and the body rides along
    // either way. On a rare symbol the faithful output is tiny and the guard
    // always fires, which is exactly the case where the attached source is
    // worth the most; dropping it there would have made point A a no-op on the
    // queries it targets.
    if !a.no_guard
        && out.total_matches > 0
        && rendered.len() as u64 >= out.faithful_bytes
    {
        let code = executor::passthrough(&plan.rg_args, usize::MAX).unwrap_or(2);
        if let Some(b) = attached {
            print!("{}", body::render(&b));
        }
        std::process::exit(code);
    }

    print!("{rendered}");
    if let Some(b) = attached {
        print!("{}", body::render(&b));
    }
    std::process::exit(if out.total_matches > 0 { 0 } else { 1 });
}

fn to_json(out: &executor::Outcome, query: &str, repro: &str, ms: u128) -> String {
    let files: Vec<serde_json::Value> = out
        .files
        .iter()
        .map(|f| {
            let l = f.def.as_ref().or_else(|| f.best.first());
            serde_json::json!({
                "path": f.path,
                "matches": f.matches,
                "score": f.score,
                "is_def": f.def.is_some(),
                "line": l.map(|l| l.n),
                "text": l.map(|l| l.text.clone()),
            })
        })
        .collect();
    serde_json::to_string_pretty(&serde_json::json!({
        "query": query,
        "hits": out.total_matches,
        "files": out.total_files,
        "elapsed_ms": ms,
        "faithful_bytes": out.faithful_bytes,
        "repro": repro,
        "results": files,
    }))
    .unwrap_or_default()
        + "\n"
}
