//! The model catalog, read from the models.dev cache krowk already keeps
//! for prices (R-PROV-2): a model's family, its context window and output
//! cap, whether it reasons and at which efforts, whether it calls tools, and
//! which wire API it is served on. Prices come from the same file through
//! the host's `Pricer`, which the CLI owns.
//!
//! The wire API is read the way models.dev records it — the AI SDK package
//! a provider (or one model of it) is served through, and a model's own
//! `shape` where the provider serves two:
//!
//! | models.dev | wire API |
//! |---|---|
//! | `shape: "responses"` | `openai-responses` |
//! | `shape: "completions"` | `chat-completions` |
//! | `@ai-sdk/openai` | `openai-responses` |
//! | `@ai-sdk/anthropic`, `…/anthropic` | `anthropic-messages` |
//! | `@ai-sdk/xai`, `@ai-sdk/openai-compatible`, `@openrouter/ai-sdk-provider` | `chat-completions` |
//! | anything else | none krowk speaks |

use crate::protocol::{Effort, WireApi};
use serde::Deserialize;
use serde_json::value::RawValue;
use std::collections::HashMap;

/// What the catalog knows of one model.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ModelInfo {
    /// e.g. `gpt-codex`, `grok`, `claude-opus`: picks the toolset preset.
    pub family: Option<String>,
    pub context_window: Option<u64>,
    pub max_output: Option<u64>,
    pub reasoning: bool,
    /// The efforts the model takes, on krowk's ladder. Empty for a model
    /// that reasons with no effort to choose.
    pub efforts: Vec<Effort>,
    pub tool_call: bool,
    /// The wire API it is served on, when it is one krowk speaks.
    pub wire_api: Option<WireApi>,
}

#[derive(Deserialize)]
struct Provider<'a> {
    #[serde(default)]
    npm: Option<String>,
    #[serde(borrow, default)]
    models: Option<HashMap<String, &'a RawValue>>,
}

#[derive(Deserialize, Default)]
struct Model {
    #[serde(default)]
    family: Option<String>,
    #[serde(default)]
    reasoning: bool,
    #[serde(default)]
    reasoning_options: Vec<ReasoningOption>,
    #[serde(default)]
    tool_call: bool,
    #[serde(default)]
    limit: Limit,
    #[serde(default)]
    provider: Option<Served>,
}

#[derive(Deserialize, Default)]
struct ReasoningOption {
    #[serde(rename = "type", default)]
    kind: String,
    #[serde(default)]
    values: Vec<serde_json::Value>,
}

#[derive(Deserialize, Default)]
struct Limit {
    #[serde(default)]
    context: Option<u64>,
    #[serde(default)]
    output: Option<u64>,
}

#[derive(Deserialize, Default)]
struct Served {
    #[serde(default)]
    npm: Option<String>,
    #[serde(default)]
    shape: Option<String>,
}

/// The wire API an AI SDK package (and a model's `shape`) means.
pub fn wire_of(npm: Option<&str>, shape: Option<&str>) -> Option<WireApi> {
    match shape {
        Some("responses") => return Some(WireApi::OpenaiResponses),
        Some("completions") => return Some(WireApi::ChatCompletions),
        _ => {}
    }
    match npm? {
        "@ai-sdk/openai" => Some(WireApi::OpenaiResponses),
        p if p == "@ai-sdk/anthropic" || p.ends_with("/anthropic") => Some(WireApi::AnthropicMessages),
        "@ai-sdk/xai" | "@ai-sdk/openai-compatible" | "@openrouter/ai-sdk-provider" => Some(WireApi::ChatCompletions),
        _ => None,
    }
}

/// The model in a models.dev document: under its own provider first, else
/// the same id under any provider, so a router serving `gpt-5` finds
/// OpenAI's entry. Every field but the few above is skipped unread: the
/// file is megabytes.
///
/// The wire API is only ever the instance's own provider's word. How some
/// reseller serves the same id says nothing of how this instance's server
/// does — `openai/gpt-4o-mini` is on the Responses API at one gateway and on
/// Chat Completions at the next — so a borrowed entry lends its family,
/// limits and efforts, and no wire API.
pub fn lookup(raw: &[u8], provider: &str, model: &str) -> Option<ModelInfo> {
    let top: HashMap<String, &RawValue> = serde_json::from_slice(raw).ok()?;
    let of = |p: &RawValue| -> Option<ModelInfo> {
        let p: Provider = serde_json::from_str(p.get()).ok()?;
        let m: Model = serde_json::from_str(p.models?.get(model)?.get()).ok()?;
        let served = m.provider.unwrap_or_default();
        let efforts = m
            .reasoning_options
            .iter()
            .filter(|o| o.kind == "effort")
            .flat_map(|o| o.values.iter().filter_map(|v| v.as_str().and_then(Effort::parse)))
            .collect::<std::collections::BTreeSet<Effort>>()
            .into_iter()
            .collect();
        Some(ModelInfo {
            family: m.family.filter(|f| !f.is_empty()),
            context_window: m.limit.context.filter(|n| *n > 0),
            max_output: m.limit.output.filter(|n| *n > 0),
            reasoning: m.reasoning,
            efforts,
            tool_call: m.tool_call,
            wire_api: wire_of(served.npm.as_deref().or(p.npm.as_deref()), served.shape.as_deref()),
        })
    };
    if let Some(info) = top.get(provider).and_then(|p| of(p)) {
        return Some(info);
    }
    // Any provider's entry, in a fixed order so the answer does not depend
    // on hash order.
    let mut names: Vec<&String> = top.keys().collect();
    names.sort();
    names.into_iter().find_map(|n| of(top[n])).map(|info| ModelInfo { wire_api: None, ..info })
}

#[cfg(test)]
mod tests {
    use super::*;

    const DOC: &[u8] = br#"{
        "openai": {"npm": "@ai-sdk/openai", "models": {
            "gpt-5.4": {"family": "gpt", "reasoning": true, "tool_call": true,
                "reasoning_options": [{"type": "effort", "values": ["none", "low", "medium", "high", "xhigh"]}],
                "limit": {"context": 1050000, "output": 128000}, "cost": {"input": 2.5}},
            "gpt-4.1": {"family": "gpt", "reasoning": false, "tool_call": true, "limit": {"context": 1047576, "output": 32768}}
        }},
        "xai": {"npm": "@ai-sdk/xai", "models": {
            "grok-4.7": {"family": "grok", "reasoning": true, "tool_call": true,
                "reasoning_options": [{"type": "effort", "values": ["low", "medium", "high", "xhigh"]}], "limit": {"context": 500000, "output": 500000}}
        }},
        "anthropic": {"npm": "@ai-sdk/anthropic", "models": {"claude-opus-5-5": {"family": "claude-opus", "reasoning": true,
            "reasoning_options": [{"type": "effort", "values": ["low", "max", "banana"]}, {"type": "budget_tokens", "min": 1024}]}}},
        "neon": {"npm": "@ai-sdk/openai-compatible", "models": {"gpt-5-4-mini": {"family": "gpt-mini", "provider": {"npm": "@ai-sdk/openai", "shape": "responses"}}}},
        "router": {"npm": "@openrouter/ai-sdk-provider", "models": {"house": {"family": ""}}},
        "google": {"npm": "@ai-sdk/google", "models": {"gemini-3": {"family": "gemini"}}}
    }"#;

    #[test]
    fn r_prov_2_the_catalog_names_each_models_limits_efforts_and_wire_api() {
        let g = lookup(DOC, "openai", "gpt-5.4").unwrap();
        assert_eq!(g.family.as_deref(), Some("gpt"));
        assert_eq!((g.context_window, g.max_output, g.reasoning, g.tool_call), (Some(1_050_000), Some(128_000), true, true));
        assert_eq!(g.efforts, vec![Effort::None, Effort::Low, Effort::Medium, Effort::High, Effort::Xhigh]);
        assert_eq!(g.wire_api, Some(WireApi::OpenaiResponses));
        assert!(!lookup(DOC, "openai", "gpt-4.1").unwrap().reasoning);
        assert_eq!(lookup(DOC, "xai", "grok-4.7").unwrap().wire_api, Some(WireApi::ChatCompletions));
        let c = lookup(DOC, "anthropic", "claude-opus-5-5").unwrap();
        assert_eq!((c.wire_api, c.efforts.clone()), (Some(WireApi::AnthropicMessages), vec![Effort::Low, Effort::Max]), "an unknown value is skipped, the rest kept in ladder order");
        // A model's own package and shape outrank its provider's.
        assert_eq!(lookup(DOC, "neon", "gpt-5-4-mini").unwrap().wire_api, Some(WireApi::OpenaiResponses));
        assert_eq!(lookup(DOC, "router", "house").unwrap().family, None, "an empty family is none");
        assert_eq!(lookup(DOC, "google", "gemini-3").unwrap().wire_api, None, "a wire API krowk does not speak");
        // The same id under any provider, when its own does not list it —
        // its family and efforts, never its wire API.
        let borrowed = lookup(DOC, "openrouter", "grok-4.7").unwrap();
        assert_eq!((borrowed.family.as_deref(), borrowed.efforts.len(), borrowed.wire_api), (Some("grok"), 4, None));
        let reseller = lookup(DOC, "some-gateway", "gpt-5-4-mini").unwrap();
        assert_eq!((reseller.family.as_deref(), reseller.wire_api), (Some("gpt-mini"), None), "neon's Responses shape is neon's, not this gateway's");
        assert_eq!(lookup(DOC, "openai", "nope"), None);
        assert_eq!(lookup(b"not json", "openai", "gpt-5.4"), None);
        assert_eq!(wire_of(Some("@ai-sdk/google-vertex/anthropic"), None), Some(WireApi::AnthropicMessages));
        assert_eq!(wire_of(Some("@ai-sdk/openai"), Some("completions")), Some(WireApi::ChatCompletions));
    }
}
