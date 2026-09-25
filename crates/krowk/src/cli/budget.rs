//! `krowk sessions budget`: whether a session is still inside a spend limit,
//! judged by what the provider metered — never by what a request asked for.
//!
//! A request's `max_tokens` is a wish, not a meter: on 2026-09-10 two
//! qwen3.5-plus calls sent with `max_tokens: 1200` completed 1,379 and 3,422
//! tokens, and a guard summing the caps would have under-budgeted by up to
//! 3x. So the guard sums the usage blocks the store holds — every counted
//! turn's input, output, reasoning and cache tokens, a provider ledger's
//! unobserved rows included, since a request nobody saw the answer to was
//! still billed — and prices them the way `sessions show` does.
//!
//! krowk runs no model, so it cancels nothing: tripping exits 4 with
//! `budget_exceeded`, and whatever called the guard (a hook, a wrapper, a
//! CI step) is what stops the run. Stopping sending is not stopping
//! billing, which is why a zombie execution still counts here. Costs stay
//! unrounded from the turn to the comparison; only the text rounds. A cost
//! krowk cannot price trips a `--max-usd` guard rather than passing it: an
//! unknown spend is not a spend inside the limit.

use super::sessions::{self, Priced};
use super::Ctx;
use crate::output::Format;
use crate::pricing;
use krowk_api::{fail, Error};
use krowk_store::{SessionDetail, TurnDetail};
use serde::Serialize;
use serde_json::{json, Value};

/// The limits one check holds a session to; either may be absent.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Limits {
    pub usd: Option<f64>,
    pub tokens: Option<i64>,
}

/// Every metered token of the counted turns, the reasoning split kept apart
/// from output even where both price the same today.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
pub struct Metered {
    pub input: i64,
    pub output: i64,
    pub reasoning: i64,
    pub cache_read: i64,
    pub cache_write: i64,
    pub total: i64,
}

impl Metered {
    fn add(&mut self, t: &TurnDetail) {
        let n = |x: i64| x.max(0);
        self.input += n(t.input);
        self.output += n(t.output);
        self.reasoning += n(t.reasoning);
        self.cache_read += n(t.cache_read);
        self.cache_write += n(t.cache_write);
        self.total = self.input + self.output + self.reasoning + self.cache_read + self.cache_write;
    }

    /// The turns a session's own cost counts: a ledger row a transcript or an
    /// earlier export already holds is metered there.
    pub fn of(d: &SessionDetail) -> Metered {
        let mut m = Metered::default();
        for t in d.turns.iter().filter(|t| !sessions::is_observed(t)) {
            m.add(t);
        }
        m
    }
}

/// One limit the session is past.
#[derive(Debug, Clone, PartialEq)]
pub enum Trip {
    Tokens { metered: i64, limit: i64 },
    Usd { cost: f64, limit: f64 },
    UsdUnknown { unpriced: Vec<String> },
}

impl Trip {
    fn json(&self) -> Value {
        match self {
            Trip::Tokens { metered, limit } => json!({ "limit": "max_tokens", "metered": metered, "max": limit }),
            Trip::Usd { cost, limit } => json!({ "limit": "max_usd", "cost_usd": cost, "max": limit }),
            Trip::UsdUnknown { unpriced } => json!({ "limit": "max_usd", "cost_usd": Value::Null, "unpriced": unpriced }),
        }
    }

    fn sentence(&self) -> String {
        match self {
            Trip::Tokens { metered, limit } => format!("{metered} tokens metered, over --max-tokens {limit}"),
            Trip::Usd { cost, limit } => {
                format!("{} metered, over --max-usd {}", sessions::format_cost_precise(*cost), sessions::format_cost_precise(*limit))
            }
            Trip::UsdUnknown { unpriced } => format!("the cost is unknown — no price for {}", unpriced.join(", ")),
        }
    }
}

/// Every limit the metered usage is past. Strictly over trips; at the limit
/// is inside it. Compared unrounded.
pub fn check(m: &Metered, cost: &Priced, l: &Limits) -> Vec<Trip> {
    let mut out = Vec::new();
    if let Some(limit) = l.tokens
        && m.total > limit
    {
        out.push(Trip::Tokens { metered: m.total, limit });
    }
    if let Some(limit) = l.usd {
        match cost.total() {
            Some(c) if c > limit => out.push(Trip::Usd { cost: c, limit }),
            Some(_) => {}
            None => out.push(Trip::UsdUnknown { unpriced: cost.missing().into_iter().collect() }),
        }
    }
    out
}

fn limits(ctx: &Ctx) -> Result<Limits, Error> {
    let usd = match ctx.f.max_usd.trim() {
        "" => None,
        v => Some(v.parse::<f64>().ok().filter(|x| x.is_finite() && *x >= 0.0).ok_or_else(|| {
            fail("bad_flag", format!("--max-usd {v:?} is not an amount of dollars — pass a number like 0.50"))
        })?),
    };
    let tokens = match ctx.f.max_tokens.trim() {
        "" => None,
        v => Some(v.parse::<i64>().ok().filter(|x| *x >= 0).ok_or_else(|| {
            fail("bad_flag", format!("--max-tokens {v:?} is not a count of tokens — pass a whole number like 200000"))
        })?),
    };
    if usd.is_none() && tokens.is_none() {
        return Err(fail("bad_flag", "`krowk sessions budget` needs a limit: --max-usd <dollars>, --max-tokens <count>, or both"));
    }
    Ok(Limits { usd, tokens })
}

pub fn budget(ctx: &mut Ctx, args: &[String]) -> Result<(), Error> {
    sessions::check_os()?;
    let l = limits(ctx)?;
    let d = sessions::load_detail(ctx, args, "budget")?;
    let (_, cost) = sessions::price_turns(ctx, &d);
    let metered = Metered::of(&d);
    let trips = check(&metered, &cost, &l);
    let s = &d.session;
    let mut report = json!({
        "id": s.id,
        "title": s.title,
        "metered": metered,
        "cost_usd": cost.total(),
        "limits": { "max_usd": l.usd, "max_tokens": l.tokens },
        "within": trips.is_empty(),
    });
    if !cost.bases.is_empty() {
        report["priced_with"] = json!(pricing::basis_note(&cost.bases));
    }
    if !trips.is_empty() {
        report["tripped"] = Value::Array(trips.iter().map(Trip::json).collect());
        let why: Vec<String> = trips.iter().map(Trip::sentence).collect();
        let mut err = fail(
            "budget_exceeded",
            format!(
                "session {} is over budget: {} — stop the run; krowk cancels nothing, and the provider keeps billing whatever is still running",
                s.id,
                why.join("; ")
            ),
        );
        err.body.insert("details".into(), report);
        return Err(err);
    }
    if ctx.format != Format::Human {
        let summary = format!("{} — within budget", sessions::display_title(&sessions::cell(&s.title)));
        return sessions::emit_data(ctx, report, summary);
    }
    let mut line = format!("within budget  {} tokens", metered.total);
    if let Some(t) = l.tokens {
        line += &format!(" of {t}");
    }
    if let (Some(c), Some(u)) = (cost.total(), l.usd) {
        line += &format!("  {} of {}", sessions::format_cost_precise(c), sessions::format_cost_precise(u));
    }
    let _ = writeln!(ctx.io.stdout, "{line}");
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn priced(usd: Option<f64>) -> Priced {
        let mut p = Priced::default();
        match usd {
            Some(u) => p.add_known(u),
            None => p.add_unknown("p/m"),
        }
        p
    }

    fn turn(output: i64, reasoning: i64, status: &str) -> TurnDetail {
        TurnDetail { input: 84, output, reasoning, status: status.into(), ..TurnDetail::default() }
    }

    #[test]
    fn a_runaway_reasoning_turn_trips_on_what_was_metered_not_the_cap_it_was_sent_with() {
        // Sent with max_tokens 1200; the provider metered 17 output and 3,405
        // reasoning. Summing the cap would say 84 + 1200 — inside 2,000.
        let d = SessionDetail { turns: vec![turn(17, 3405, "unobserved"), turn(1379, 0, "observed")], ..SessionDetail::default() };
        let m = Metered::of(&d);
        assert_eq!((m.output, m.reasoning, m.total), (17, 3405, 84 + 17 + 3405), "the observed row is metered in its transcript");
        let trips = check(&m, &priced(Some(0.0)), &Limits { tokens: Some(2000), usd: None });
        assert_eq!(trips, vec![Trip::Tokens { metered: 3506, limit: 2000 }]);
        assert!(check(&m, &priced(Some(0.0)), &Limits { tokens: Some(3506), usd: None }).is_empty(), "at the limit is inside it");
    }

    #[test]
    fn dollars_compare_unrounded_and_an_unknown_cost_trips() {
        // Three Zen deepseek turns: 24.08 + 58.94 + 40.264 µ$. Rounded to four
        // places the total and a $0.000123 limit both read $0.0001.
        let mut p = Priced::default();
        for usd in [0.000_024_08, 0.000_058_94, 0.000_040_264] {
            p.add_known(usd);
        }
        assert_eq!(p.total(), Some(0.000_024_08 + 0.000_058_94 + 0.000_040_264), "no rounding inside the roll-up");
        let limits = Limits { usd: Some(0.000_123), tokens: None };
        assert!(matches!(check(&Metered::default(), &p, &limits)[..], [Trip::Usd { .. }]), "$0.000123284 is over $0.000123");
        assert!(check(&Metered::default(), &p, &Limits { usd: Some(0.000_124), tokens: None }).is_empty());
        let unknown = check(&Metered::default(), &priced(None), &limits);
        assert_eq!(unknown, vec![Trip::UsdUnknown { unpriced: vec!["p/m".into()] }]);
    }
}
