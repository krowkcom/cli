//! What the session is doing, told to whatever hosts the terminal: the
//! window title (`✳ krowk` waiting, `◑ <what was asked>` working, `✋` when
//! a call waits for a yes) and, inside herdr (a workspace manager for
//! coding agents, which sets `HERDR_PANE_ID`), the pane's agent state, so
//! herdr lists krowk as an agent with its status and notifies on it.

use std::process::{Command, Stdio};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    Idle,
    Working,
    Blocked,
}

impl State {
    fn herdr(self) -> &'static str {
        match self {
            State::Idle => "idle",
            State::Working => "working",
            State::Blocked => "blocked",
        }
    }
}

/// The last state told, so each change is told once.
#[derive(Default)]
pub struct Presence {
    told: Option<(State, String)>,
    pane: Option<String>,
}

impl Presence {
    /// herdr's pane, when krowk's terminal is that pane: inside tmux or
    /// screen started in it, the variable is inherited from the pane
    /// around them, and the state is not krowk's to report.
    pub fn from_env(env: &dyn Fn(&str) -> String) -> Presence {
        let pane = env("HERDR_PANE_ID");
        let nested = !env("TMUX").is_empty() || !env("STY").is_empty();
        Presence { told: None, pane: (!pane.is_empty() && !nested).then_some(pane) }
    }

    /// Gives herdr its own detection of the pane back, on the way out or
    /// into a job stop; told again from scratch afterwards.
    pub fn release(&mut self) {
        if let Some(pane) = &self.pane
            && self.told.is_some()
        {
            herdr(&["pane", "release-agent", pane, "--source", "krowk", "--agent", "krowk"]);
        }
        self.told = None;
    }

    /// The title to set now, if `state` and `topic` differ from what was
    /// last told; herdr is told in the background, and never waited on.
    pub fn update(&mut self, state: State, topic: &str) -> Option<String> {
        let topic: String = topic.lines().next().unwrap_or_default().chars().filter(|c| !c.is_control()).take(48).collect();
        let now = (state, topic);
        if self.told.as_ref() == Some(&now) {
            return None;
        }
        let state_changed = self.told.as_ref().is_none_or(|(s, _)| *s != now.0);
        if state_changed && let Some(pane) = &self.pane {
            herdr(&["pane", "report-agent", pane, "--source", "krowk", "--agent", "krowk", "--state", now.0.herdr()]);
        }
        let title = match (now.0, now.1.is_empty()) {
            (State::Idle, _) | (_, true) => match now.0 {
                State::Idle => "✳ krowk".to_string(),
                State::Working => "◑ krowk".to_string(),
                State::Blocked => "✋ krowk".to_string(),
            },
            (State::Working, false) => format!("◑ {}", now.1),
            (State::Blocked, false) => format!("✋ {}", now.1),
        };
        self.told = Some(now);
        Some(title)
    }
}

/// Fire and forget: a missing or slow herdr costs nothing here.
fn herdr(args: &[&str]) {
    let _ = Command::new("herdr").args(args).stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null()).spawn();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn each_change_is_told_once_and_the_title_names_what_was_asked() {
        let mut p = Presence::from_env(&|_| String::new());
        assert_eq!(p.update(State::Idle, "").as_deref(), Some("✳ krowk"));
        assert_eq!(p.update(State::Idle, ""), None, "nothing new");
        assert_eq!(p.update(State::Working, "fix the failing test\nand more").as_deref(), Some("◑ fix the failing test"));
        assert_eq!(p.update(State::Blocked, "fix the failing test").as_deref(), Some("✋ fix the failing test"));
        assert_eq!(p.update(State::Working, "evil\x1b]0;x\x07").as_deref(), Some("◑ evil]0;x"), "no escape reaches the title");
    }

    #[test]
    fn herdr_is_told_only_about_its_own_pane() {
        let env = |pairs: &'static [(&'static str, &'static str)]| move |k: &str| pairs.iter().find(|(n, _)| *n == k).map(|(_, v)| v.to_string()).unwrap_or_default();
        assert_eq!(Presence::from_env(&env(&[("HERDR_PANE_ID", "w1:p2")])).pane.as_deref(), Some("w1:p2"));
        assert_eq!(Presence::from_env(&env(&[("HERDR_PANE_ID", "w1:p2"), ("TMUX", "/tmp/tmux-1000/default,1,0")])).pane, None, "tmux inside the pane");
        assert_eq!(Presence::from_env(&env(&[("HERDR_PANE_ID", "w1:p2"), ("STY", "123.pts-0")])).pane, None, "screen inside the pane");
        assert_eq!(Presence::from_env(&env(&[])).pane, None);
    }
}
