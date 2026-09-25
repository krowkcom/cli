//! The instance registry: named provider instances in krowk's own config
//! (`config.json`, key `instances`), each one account or configuration of a
//! provider on this host — `anthropic`, `anthropic:work`. A model is always
//! chosen as an instance and a model id (`ModelRef`).
//!
//! A definition holds nothing secret: an API-key instance names the
//! environment variable its key is read from, never the key, so definitions
//! can sync between hosts and keys cannot (R-INST-5). Only the Anthropic
//! API-key kind exists so far; later kinds (other native providers, the
//! vendor backends) are more variants of `InstanceKind`.
//!
//! With nothing configured there is still one instance, `anthropic`, reading
//! `ANTHROPIC_API_KEY` and `ANTHROPIC_BASE_URL` — the conventional names, so
//! a machine already set up for Anthropic's tools works unconfigured.

use crate::protocol::{ModelRef, WireApi};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

pub const DEFAULT_INSTANCE: &str = "anthropic";
/// The model a session runs on when neither the command line, the session
/// nor the config names one.
pub const DEFAULT_MODEL: &str = "claude-opus-5";
pub const ANTHROPIC_API_URL: &str = "https://api.anthropic.com";
const ANTHROPIC_KEY_ENV: &str = "ANTHROPIC_API_KEY";
const ANTHROPIC_BASE_URL_ENV: &str = "ANTHROPIC_BASE_URL";

/// The harness's part of `config.json`. Keys it does not know are kept by
/// whoever rewrites the file, and ignored here.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct InstancesConfig {
    #[serde(default)]
    pub instances: BTreeMap<String, InstanceKind>,
    /// `<instance>/<model>`, or a bare model id on the default instance.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub default_model: Option<String>,
    /// The toolset preset every model runs with (`claude`, `gpt`, `grok`),
    /// instead of the one its family picks. `--toolset` overrides it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub toolset: Option<String>,
}

/// What an instance is. Tagged by `kind`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(tag = "kind", rename_all_fields = "camelCase")]
pub enum InstanceKind {
    /// The Anthropic Messages API with an API key.
    #[serde(rename = "anthropic-api")]
    AnthropicApi {
        /// The environment variable holding the key; `ANTHROPIC_API_KEY`
        /// when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        api_key_env: Option<String>,
        /// Where the API is; `https://api.anthropic.com` when absent. A
        /// router speaking the same wire API goes here.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        base_url: Option<String>,
        /// `adaptive` (the default) or `off`. Off omits the parameter, for
        /// models that take no thinking configuration.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        thinking: Option<Thinking>,
        /// The output cap per model call; 32000 when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        max_tokens: Option<u32>,
    },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub enum Thinking {
    Adaptive,
    Off,
}

/// An instance ready to use: its definition with the secret read.
#[derive(Clone, PartialEq)]
pub struct Resolved {
    pub name: String,
    pub provider: &'static str,
    pub wire_api: WireApi,
    pub api_key: String,
    /// Which variable the key was, or should have been, read from — named in
    /// the fix when it is missing.
    pub api_key_env: String,
    pub base_url: String,
    pub thinking: Thinking,
    pub max_tokens: u32,
}

// Hand-written so a key never reaches a log line through `{:?}`.
impl std::fmt::Debug for Resolved {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Resolved")
            .field("name", &self.name)
            .field("base_url", &self.base_url)
            .field("api_key", &if self.api_key.is_empty() { "<unset>" } else { "<set>" })
            .finish()
    }
}

/// Every instance this host has, resolved against the environment once, up
/// front: the engine never reads the environment itself.
#[derive(Debug, Clone, Default)]
pub struct Registry {
    pub instances: BTreeMap<String, Resolved>,
    pub default_model: Option<String>,
    /// Config's `toolset`, already known to name a preset.
    pub toolset: Option<String>,
}

impl Registry {
    /// The config's instances, plus the implicit `anthropic` one when the
    /// config does not define an instance of that name.
    pub fn resolve(cfg: &InstancesConfig, env: &dyn Fn(&str) -> String) -> Registry {
        let mut instances = BTreeMap::new();
        let implicit = InstanceKind::AnthropicApi { api_key_env: None, base_url: None, thinking: None, max_tokens: None };
        let defined = cfg.instances.iter().map(|(n, k)| (n.as_str(), k));
        for (name, kind) in std::iter::once((DEFAULT_INSTANCE, &implicit)).chain(defined) {
            instances.insert(name.to_string(), resolve_one(name, kind, env));
        }
        Registry { instances, default_model: cfg.default_model.clone(), toolset: cfg.toolset.clone() }
    }

    /// `--model`'s reading: `<instance>/<model>` when the part before the
    /// first `/` names an instance, else the whole string is a model id on
    /// the default instance — a router's model ids have slashes of their own.
    pub fn parse_model(&self, s: &str) -> Result<ModelRef, String> {
        let s = s.trim();
        if s.is_empty() {
            return Err("the model is empty — pass `<instance>/<model>` or a model id".into());
        }
        if let Some((instance, model)) = s.split_once('/')
            && self.instances.contains_key(instance)
        {
            if model.is_empty() {
                return Err(format!("{s:?} names the instance {instance:?} but no model — e.g. {instance}/{DEFAULT_MODEL}"));
            }
            return Ok(ModelRef { instance: instance.into(), model: model.into() });
        }
        Ok(ModelRef { instance: DEFAULT_INSTANCE.into(), model: s.into() })
    }

    /// The configured default, else krowk's.
    pub fn default_model(&self) -> Result<ModelRef, String> {
        match &self.default_model {
            Some(m) => self.parse_model(m).map_err(|e| format!("config defaultModel: {e}")),
            None => Ok(ModelRef { instance: DEFAULT_INSTANCE.into(), model: DEFAULT_MODEL.into() }),
        }
    }

    pub fn get(&self, name: &str) -> Result<&Resolved, String> {
        self.instances.get(name).ok_or_else(|| {
            let known: Vec<&str> = self.instances.keys().map(String::as_str).collect();
            format!("no instance named {name:?} — this host has {}; add one under \"instances\" in krowk's config.json", known.join(", "))
        })
    }
}

fn resolve_one(name: &str, kind: &InstanceKind, env: &dyn Fn(&str) -> String) -> Resolved {
    match kind {
        InstanceKind::AnthropicApi { api_key_env, base_url, thinking, max_tokens } => {
            let key_env = api_key_env.clone().unwrap_or_else(|| ANTHROPIC_KEY_ENV.into());
            // The conventional base-URL variable only speaks for the
            // instance that reads the conventional key.
            let base = base_url.clone().filter(|b| !b.trim().is_empty()).unwrap_or_else(|| {
                let from_env = if key_env == ANTHROPIC_KEY_ENV { env(ANTHROPIC_BASE_URL_ENV) } else { String::new() };
                if from_env.trim().is_empty() { ANTHROPIC_API_URL.into() } else { from_env }
            });
            Resolved {
                name: name.into(),
                provider: "anthropic",
                wire_api: WireApi::AnthropicMessages,
                api_key: env(&key_env).trim().to_string(),
                api_key_env: key_env,
                base_url: base.trim().trim_end_matches('/').to_string(),
                thinking: thinking.unwrap_or(Thinking::Adaptive),
                max_tokens: max_tokens.unwrap_or(32_000),
            }
        }
    }
}

/// The harness's part of a `config.json` document. A file that is not JSON
/// is the caller's error to report; a document whose `instances` are
/// malformed is an error here, since somebody wrote them deliberately.
pub fn from_config_json(raw: &serde_json::Value) -> Result<InstancesConfig, String> {
    let mut cfg = InstancesConfig::default();
    if let Some(v) = raw.get("instances") {
        cfg.instances = serde_json::from_value(v.clone()).map_err(|e| format!("\"instances\": {e}"))?;
    }
    if let Some(v) = raw.get("defaultModel") {
        cfg.default_model = Some(v.as_str().ok_or("\"defaultModel\" must be a string")?.to_string());
    }
    if let Some(v) = raw.get("toolset") {
        let name = v.as_str().ok_or("\"toolset\" must be a string")?;
        if crate::toolset::by_name(name).is_none() {
            return Err(format!("\"toolset\": {name:?} is not a toolset — one of {}", crate::toolset::names().join(", ")));
        }
        cfg.toolset = Some(name.to_string());
    }
    Ok(cfg)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_inst_a_bare_config_still_has_the_anthropic_instance_and_models_parse_either_way() {
        let env = |k: &str| match k {
            "ANTHROPIC_API_KEY" => "sk-test".into(),
            "ANTHROPIC_BASE_URL" => "http://127.0.0.1:9/".into(),
            "WORK_KEY" => "sk-work".into(),
            _ => String::new(),
        };
        let cfg = from_config_json(&serde_json::json!({
            "workspace": "ws_x",
            "instances": {"anthropic:work": {"kind": "anthropic-api", "apiKeyEnv": "WORK_KEY"}},
        }))
        .unwrap();
        let reg = Registry::resolve(&cfg, &env);
        let a = reg.get("anthropic").unwrap();
        assert_eq!((a.api_key.as_str(), a.base_url.as_str()), ("sk-test", "http://127.0.0.1:9"));
        let w = reg.get("anthropic:work").unwrap();
        assert_eq!((w.api_key.as_str(), w.base_url.as_str()), ("sk-work", ANTHROPIC_API_URL), "the conventional base URL is the default instance's");
        assert!(!format!("{w:?}").contains("sk-work"), "a key never prints");
        assert_eq!(reg.parse_model("anthropic:work/claude-x").unwrap(), ModelRef { instance: "anthropic:work".into(), model: "claude-x".into() });
        assert_eq!(reg.parse_model("claude-x").unwrap().instance, "anthropic");
        assert_eq!(reg.parse_model("openrouter/some/model").unwrap(), ModelRef { instance: "anthropic".into(), model: "openrouter/some/model".into() });
        assert!(reg.parse_model("anthropic/").is_err());
        assert!(reg.get("nope").unwrap_err().contains("anthropic, anthropic:work"));
        assert_eq!(reg.default_model().unwrap().model, DEFAULT_MODEL);
        assert!(from_config_json(&serde_json::json!({"instances": {"x": {"kind": "martian"}}})).is_err());
        // R-TOOL-2: config can pin a toolset, and only one that exists.
        assert_eq!(Registry::resolve(&from_config_json(&serde_json::json!({"toolset": "grok"})).unwrap(), &env).toolset.as_deref(), Some("grok"));
        assert!(from_config_json(&serde_json::json!({"toolset": "vim"})).unwrap_err().contains("claude, gpt, grok"));
    }
}
