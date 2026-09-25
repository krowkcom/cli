//! The TUI's part of krowk's config.json, under `"tui"` (R-TUI-2):
//!
//! ```json
//! { "tui": { "statusBar": true, "statusItems": ["model", "instance", "cost", "connectivity"] } }
//! ```
//!
//! - `statusBar` — false hides the status bar. The "no network
//!   connectivity" notice is not part of it and shows regardless (R-OFF-1).
//! - `statusItems` — which items the bar shows, in order: any of `model`,
//!   `instance`, `cost`, `connectivity`, `session`.
//!
//! The overlays are toggled from the keyboard rather than configured: `?` on
//! an empty prompt for the keys, Ctrl-O for the session's details.

use serde_json::Value;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Item {
    Model,
    Instance,
    Cost,
    Connectivity,
    Session,
}

impl Item {
    pub const ALL: [(&'static str, Item); 5] =
        [("model", Item::Model), ("instance", Item::Instance), ("cost", Item::Cost), ("connectivity", Item::Connectivity), ("session", Item::Session)];

    fn parse(s: &str) -> Option<Item> {
        Item::ALL.iter().find(|(n, _)| *n == s).map(|(_, i)| *i)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Settings {
    pub status_bar: bool,
    pub status_items: Vec<Item>,
}

impl Default for Settings {
    fn default() -> Settings {
        Settings { status_bar: true, status_items: vec![Item::Model, Item::Instance, Item::Cost, Item::Connectivity] }
    }
}

/// Reads `"tui"` from a parsed config.json. Something written wrong is
/// reported, and the rest still applies: a typo in a status item should not
/// keep anybody from their prompt.
pub fn from_config(raw: &Value) -> (Settings, Vec<String>) {
    let mut s = Settings::default();
    let mut warnings = Vec::new();
    let Some(tui) = raw.get("tui") else { return (s, warnings) };
    let Some(tui) = tui.as_object() else {
        warnings.push("config \"tui\" must be an object — using the defaults".into());
        return (s, warnings);
    };
    match tui.get("statusBar") {
        None => {}
        Some(Value::Bool(b)) => s.status_bar = *b,
        Some(_) => warnings.push("config tui.statusBar must be true or false".into()),
    }
    match tui.get("statusItems") {
        None => {}
        Some(Value::Array(items)) => {
            s.status_items.clear();
            for v in items {
                match v.as_str().and_then(Item::parse) {
                    Some(i) if !s.status_items.contains(&i) => s.status_items.push(i),
                    Some(_) => {}
                    None => warnings.push(format!(
                        "config tui.statusItems: {v} is not an item — one of {}",
                        Item::ALL.iter().map(|(n, _)| *n).collect::<Vec<_>>().join(", ")
                    )),
                }
            }
        }
        Some(_) => warnings.push("config tui.statusItems must be a list of item names".into()),
    }
    for key in tui.keys() {
        if !matches!(key.as_str(), "statusBar" | "statusItems") {
            warnings.push(format!("config tui.{key} is not a setting — the TUI reads statusBar and statusItems"));
        }
    }
    (s, warnings)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn r_tui_2_the_status_bar_is_optional_and_its_items_configurable() {
        assert_eq!(from_config(&json!({})).0, Settings::default());
        let (s, w) = from_config(&json!({"tui": {"statusBar": false}}));
        assert!(!s.status_bar && w.is_empty());
        let (s, w) = from_config(&json!({"tui": {"statusItems": ["cost", "model", "cost", "nope"]}}));
        assert_eq!(s.status_items, [Item::Cost, Item::Model], "in the order given, once each");
        assert_eq!(w.len(), 1, "{w:?}");
        assert!(w[0].contains("\"nope\"") && w[0].contains("connectivity"), "{w:?}");
        let (s, w) = from_config(&json!({"tui": {"statusBar": "yes", "colour": 1}}));
        assert!(s.status_bar, "a malformed value leaves the default");
        assert_eq!(w.len(), 2, "{w:?}");
    }
}
