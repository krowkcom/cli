//! The instance registry: named provider instances in krowk's own config
//! (`config.json`, key `instances`), each one account or configuration of a
//! provider on this host — `anthropic`, `openai:work`, `supergrok`. A model
//! is always chosen as an instance and a model id (`ModelRef`).
//!
//! A definition holds nothing secret: an API-key instance names the
//! environment variable its key is read from, never the key, and an OAuth
//! instance's tokens live in krowk's provider credentials file, so
//! definitions can sync between hosts and keys cannot (R-INST-5). The kinds
//! are the native providers' (R-PROV-4): `anthropic-api`, `openai-api`,
//! `xai-api`, `openrouter-api`, `openai-compatible` for anything else that
//! speaks Chat Completions, and `xai-oauth` for a SuperGrok subscription.
//! The vendor backends are more variants: `claude-code` drives the user's
//! own `claude` binary (R-BACK-1), and is a binary path, a config directory
//! (`CLAUDE_CONFIG_DIR`), environment variables and launch arguments
//! (R-INST-1) — so `claude:work` and `claude:personal` are two Claude
//! accounts on one host, each logged in through Claude's own flow.
//!
//! With nothing configured there is still one instance per provider —
//! `anthropic`, `openai`, `xai`, `openrouter`, reading the conventional key
//! variables, so a machine already set up for a provider's own tools works
//! unconfigured, `supergrok`, which needs only a login, and `claude`, the
//! `claude` on PATH with Claude's own default config directory.

use crate::protocol::{Effort, ModelRef, WireApi};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::PathBuf;

pub const DEFAULT_INSTANCE: &str = "anthropic";
/// The model a session runs on when neither the command line, the session
/// nor the config names one.
pub const DEFAULT_MODEL: &str = "claude-opus-5-5";
pub const ANTHROPIC_API_URL: &str = "https://api.anthropic.com";
pub const OPENAI_API_URL: &str = "https://api.openai.com/v1";
pub const XAI_API_URL: &str = "https://api.x.ai/v1";
pub const OPENROUTER_API_URL: &str = "https://openrouter.ai/api/v1";
/// xAI's authorization server, whose metadata names the endpoints the
/// SuperGrok login uses.
pub const XAI_ISSUER: &str = "https://auth.x.ai";
const ANTHROPIC_KEY_ENV: &str = "ANTHROPIC_API_KEY";
const ANTHROPIC_BASE_URL_ENV: &str = "ANTHROPIC_BASE_URL";
const OPENAI_KEY_ENV: &str = "OPENAI_API_KEY";
const OPENAI_BASE_URL_ENV: &str = "OPENAI_BASE_URL";
const XAI_KEY_ENV: &str = "XAI_API_KEY";
const OPENROUTER_KEY_ENV: &str = "OPENROUTER_API_KEY";

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

/// What an instance is. Tagged by `kind`. Every API-key kind names the
/// environment variable its key is read from, never the key.
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
        /// The reasoning effort every turn asks for unless `--effort` says
        /// otherwise; the provider's default when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        effort: Option<Effort>,
    },
    /// OpenAI with an API key: the Responses API, or Chat Completions for a
    /// model the catalog says is served there.
    #[serde(rename = "openai-api")]
    OpenaiApi {
        /// `OPENAI_API_KEY` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        api_key_env: Option<String>,
        /// `https://api.openai.com/v1` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        base_url: Option<String>,
        /// Pins one wire API for every model, instead of the catalog's.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        wire_api: Option<WireApi>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        effort: Option<Effort>,
    },
    /// xAI with an API key, over Chat Completions.
    #[serde(rename = "xai-api")]
    XaiApi {
        /// `XAI_API_KEY` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        api_key_env: Option<String>,
        /// `https://api.x.ai/v1` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        base_url: Option<String>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        effort: Option<Effort>,
    },
    /// OpenRouter with an API key, over Chat Completions.
    #[serde(rename = "openrouter-api")]
    OpenrouterApi {
        /// `OPENROUTER_API_KEY` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        api_key_env: Option<String>,
        /// `https://openrouter.ai/api/v1` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        base_url: Option<String>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        effort: Option<Effort>,
    },
    /// Any server that speaks Chat Completions (or the Responses API) at a
    /// base URL: a local model, a gateway, a provider krowk has no kind for.
    #[serde(rename = "openai-compatible")]
    OpenaiCompatible {
        /// Where the API is, e.g. `http://127.0.0.1:11434/v1`. Required.
        base_url: String,
        /// The environment variable holding the key; none is sent when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        api_key_env: Option<String>,
        /// The models.dev provider id its models are priced and described
        /// under; `openai-compatible` when absent, which prices nothing.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        provider: Option<String>,
        /// `chat-completions` (the default) or `openai-responses`.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        wire_api: Option<WireApi>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        effort: Option<Effort>,
    },
    /// xAI with a SuperGrok (or X Premium) subscription, signed in with
    /// OAuth by `krowk providers add supergrok`. The tokens live in krowk's
    /// provider credentials file, never here.
    #[serde(rename = "xai-oauth")]
    XaiOauth {
        /// `https://api.x.ai/v1` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        base_url: Option<String>,
        /// The authorization server; `https://auth.x.ai` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        issuer: Option<String>,
        /// The OAuth client krowk signs in as. When absent, krowk registers
        /// itself where the server offers dynamic registration.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        client_id: Option<String>,
        /// The scopes asked for; `openid offline_access` when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        scope: Option<String>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        effort: Option<Effort>,
    },
    /// The user's own, unmodified `claude` binary — Claude Code — driven as
    /// a backend: it runs the loop on its own login, a Claude subscription
    /// or whatever the config directory holds, and krowk never reads that
    /// login (R-BACK-2). Added with `krowk providers add claude`.
    #[serde(rename = "claude-code")]
    ClaudeCode {
        /// The binary; `claude` on PATH when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        binary: Option<String>,
        /// `CLAUDE_CONFIG_DIR`: where Claude Code keeps this account's
        /// login, settings and transcripts. Claude's own default
        /// (`~/.claude`) when absent.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        config_dir: Option<String>,
        /// More environment for the process, e.g. `ANTHROPIC_BASE_URL` for
        /// a router. Literal values: a key belongs in the environment krowk
        /// runs in, never in a definition.
        #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
        env: BTreeMap<String, String>,
        /// More launch arguments, after krowk's own.
        #[serde(default, skip_serializing_if = "Vec::is_empty")]
        args: Vec<String>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        effort: Option<Effort>,
    },
}

impl InstanceKind {
    /// The `kind` tag, as config spells it.
    pub fn tag(&self) -> &'static str {
        match self {
            InstanceKind::AnthropicApi { .. } => "anthropic-api",
            InstanceKind::OpenaiApi { .. } => "openai-api",
            InstanceKind::XaiApi { .. } => "xai-api",
            InstanceKind::OpenrouterApi { .. } => "openrouter-api",
            InstanceKind::OpenaiCompatible { .. } => "openai-compatible",
            InstanceKind::XaiOauth { .. } => "xai-oauth",
            InstanceKind::ClaudeCode { .. } => "claude-code",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub enum Thinking {
    Adaptive,
    Off,
}

/// How an instance's calls are authorized.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Auth {
    /// A key from the environment variable `api_key_env`.
    ApiKey,
    /// Nothing: a local server that wants no key.
    Keyless,
    /// Tokens from the OAuth login, refreshed as they expire.
    OAuth { issuer: String, client_id: Option<String>, scope: String },
    /// The vendor binary's own login, in its config directory. krowk asks
    /// the binary whether there is one (`claude auth status`) and never
    /// reads it (R-BACK-2, R-INST-2).
    Vendor,
}

/// How a backend instance's process is started (R-INST-1).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Backend {
    /// The binary, as configured: a path, or a name looked up on PATH.
    pub binary: String,
    /// Where it was found; none when it is not there.
    pub path: Option<PathBuf>,
    /// `CLAUDE_CONFIG_DIR`, set for the process; none leaves the vendor's
    /// own default.
    pub config_dir: Option<PathBuf>,
    /// The config directory the process will use, set or not — where its
    /// transcripts are. None when there is no home to find it in.
    pub home: Option<PathBuf>,
    pub env: BTreeMap<String, String>,
    pub args: Vec<String>,
}

/// An instance ready to use: its definition with the secret read.
#[derive(Clone, PartialEq)]
pub struct Resolved {
    pub name: String,
    /// The `kind` it was defined as.
    pub kind: &'static str,
    /// The models.dev provider id its models are priced and described
    /// under, and the provider its reasoning blobs replay to.
    pub provider: String,
    /// The provider as a person names it, for the words of a failure.
    pub vendor: &'static str,
    /// The wire API a model is served on when the catalog does not say.
    pub wire_api: WireApi,
    /// The wire APIs it can serve a model on; the catalog picks among them
    /// unless the definition pins one.
    pub wires: &'static [WireApi],
    pub wire_pinned: bool,
    pub auth: Auth,
    pub api_key: String,
    /// Which variable the key was, or should have been, read from — named in
    /// the fix when it is missing.
    pub api_key_env: String,
    pub base_url: String,
    pub thinking: Thinking,
    pub max_tokens: u32,
    pub effort: Option<Effort>,
    /// Set on a backend instance: the process krowk drives.
    pub backend: Option<Backend>,
}

// Hand-written so a key never reaches a log line through `{:?}`.
impl std::fmt::Debug for Resolved {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Resolved")
            .field("name", &self.name)
            .field("kind", &self.kind)
            .field("base_url", &self.base_url)
            .field("api_key", &if self.api_key.is_empty() { "<unset>" } else { "<set>" })
            .finish()
    }
}

impl Resolved {
    /// The wire API a model runs on: the catalog's when this instance can
    /// speak it and does not pin one, else the instance's own.
    pub fn wire_for(&self, catalog: Option<WireApi>) -> WireApi {
        match catalog {
            Some(w) if !self.wire_pinned && self.wires.contains(&w) => w,
            _ => self.wire_api,
        }
    }

    /// Why a call cannot be made, before one is: an API-key instance whose
    /// variable is unset.
    pub fn missing_key(&self) -> Option<String> {
        (self.auth == Auth::ApiKey && self.api_key.is_empty()).then(|| {
            format!("no API key for the {} instance — set {} (krowk reads the key from the environment, never from a file)", self.name, self.api_key_env)
        })
    }
}

/// The instances every host has without configuring any: one per provider
/// with an API-key kind, reading the conventional variable, SuperGrok,
/// which needs only a login, and `claude`, the Claude Code on PATH with its
/// own default config directory. A config entry of the same name replaces one.
pub fn implicit() -> Vec<(&'static str, InstanceKind)> {
    vec![
        ("anthropic", InstanceKind::AnthropicApi { api_key_env: None, base_url: None, thinking: None, max_tokens: None, effort: None }),
        ("openai", InstanceKind::OpenaiApi { api_key_env: None, base_url: None, wire_api: None, effort: None }),
        ("xai", InstanceKind::XaiApi { api_key_env: None, base_url: None, effort: None }),
        ("openrouter", InstanceKind::OpenrouterApi { api_key_env: None, base_url: None, effort: None }),
        ("supergrok", InstanceKind::XaiOauth { base_url: None, issuer: None, client_id: None, scope: None, effort: None }),
        ("claude", InstanceKind::ClaudeCode { binary: None, config_dir: None, env: BTreeMap::new(), args: Vec::new(), effort: None }),
    ]
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
    /// The config's instances, plus the implicit ones the config does not
    /// define an instance of that name for.
    pub fn resolve(cfg: &InstancesConfig, env: &dyn Fn(&str) -> String) -> Registry {
        let mut instances = BTreeMap::new();
        for (name, kind) in implicit() {
            instances.insert(name.to_string(), resolve_one(name, &kind, env));
        }
        for (name, kind) in &cfg.instances {
            instances.insert(name.clone(), resolve_one(name, kind, env));
        }
        Registry { instances, default_model: cfg.default_model.clone(), toolset: cfg.toolset.clone() }
    }

    /// `--model`'s reading: `<instance>/<model>` when the part before the
    /// first `/` names an instance — a router's model ids have slashes of
    /// their own. A bare id with no `/` goes to the implicit instance of the
    /// provider its family names (`gpt-…`, `o3`, `codex-…` to `openai`,
    /// `grok-…` to `xai`), and anything else to `anthropic`.
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
        let by_family = match (s.contains('/'), crate::toolset::family_from_id(s)) {
            (false, Some("gpt" | "o" | "codex")) => "openai",
            (false, Some("grok")) => "xai",
            _ => DEFAULT_INSTANCE,
        };
        Ok(ModelRef { instance: by_family.into(), model: s.into() })
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
            format!("no instance named {name:?} — this host has {}; add one with `krowk providers add`", known.join(", "))
        })
    }
}

/// A binary as the shell finds it: a path is taken as it is, a bare name
/// is looked up on PATH.
pub fn find_binary(binary: &str, path: &str) -> Option<PathBuf> {
    let runnable = |p: &std::path::Path| {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            p.metadata().is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
        }
        #[cfg(not(unix))]
        p.is_file()
    };
    if binary.contains(std::path::MAIN_SEPARATOR) || binary.contains('/') {
        let p = PathBuf::from(binary);
        return runnable(&p).then_some(p);
    }
    std::env::split_paths(path).map(|d| d.join(binary)).find(|p| runnable(p))
}

fn clean_url(u: &str) -> String {
    u.trim().trim_end_matches('/').to_string()
}

const RESPONSES_OR_CHAT: &[WireApi] = &[WireApi::OpenaiResponses, WireApi::ChatCompletions];
const CHAT: &[WireApi] = &[WireApi::ChatCompletions];
const CLAUDE_CODE: &[WireApi] = &[WireApi::ClaudeCode];

fn resolve_one(name: &str, kind: &InstanceKind, env: &dyn Fn(&str) -> String) -> Resolved {
    // A base URL from config, else (for the instance reading the
    // conventional key only) the conventional variable, else the default.
    let base = |configured: &Option<String>, key_env: &str, conventional: (&str, &str), default: &str| -> String {
        let from_env = if key_env == conventional.0 && !conventional.1.is_empty() { env(conventional.1) } else { String::new() };
        let url = configured.clone().filter(|b| !b.trim().is_empty()).unwrap_or(if from_env.trim().is_empty() { default.to_string() } else { from_env });
        clean_url(&url)
    };
    let api = |key_env: &Option<String>, conventional: &str| -> (String, String) {
        let key_env = key_env.clone().filter(|k| !k.trim().is_empty()).unwrap_or_else(|| conventional.into());
        (env(&key_env).trim().to_string(), key_env)
    };
    let template = |provider: &str, vendor: &'static str, wire: WireApi, wires: &'static [WireApi], effort: Option<Effort>| Resolved {
        name: name.into(),
        kind: kind.tag(),
        provider: provider.into(),
        vendor,
        wire_api: wire,
        wires,
        wire_pinned: false,
        auth: Auth::ApiKey,
        api_key: String::new(),
        api_key_env: String::new(),
        base_url: String::new(),
        thinking: Thinking::Adaptive,
        max_tokens: 32_000,
        effort,
        backend: None,
    };
    match kind {
        InstanceKind::AnthropicApi { api_key_env, base_url, thinking, max_tokens, effort } => {
            let (api_key, key_env) = api(api_key_env, ANTHROPIC_KEY_ENV);
            Resolved {
                base_url: base(base_url, &key_env, (ANTHROPIC_KEY_ENV, ANTHROPIC_BASE_URL_ENV), ANTHROPIC_API_URL),
                api_key,
                api_key_env: key_env,
                thinking: thinking.unwrap_or(Thinking::Adaptive),
                max_tokens: max_tokens.unwrap_or(32_000),
                ..template("anthropic", "Anthropic", WireApi::AnthropicMessages, &[WireApi::AnthropicMessages], *effort)
            }
        }
        InstanceKind::OpenaiApi { api_key_env, base_url, wire_api, effort } => {
            let (api_key, key_env) = api(api_key_env, OPENAI_KEY_ENV);
            let wire = wire_api.filter(|w| RESPONSES_OR_CHAT.contains(w));
            Resolved {
                base_url: base(base_url, &key_env, (OPENAI_KEY_ENV, OPENAI_BASE_URL_ENV), OPENAI_API_URL),
                api_key,
                api_key_env: key_env,
                wire_pinned: wire.is_some(),
                ..template("openai", "OpenAI", wire.unwrap_or(WireApi::OpenaiResponses), RESPONSES_OR_CHAT, *effort)
            }
        }
        InstanceKind::XaiApi { api_key_env, base_url, effort } => {
            let (api_key, key_env) = api(api_key_env, XAI_KEY_ENV);
            Resolved { base_url: base(base_url, &key_env, ("", ""), XAI_API_URL), api_key, api_key_env: key_env, ..template("xai", "xAI", WireApi::ChatCompletions, CHAT, *effort) }
        }
        InstanceKind::OpenrouterApi { api_key_env, base_url, effort } => {
            let (api_key, key_env) = api(api_key_env, OPENROUTER_KEY_ENV);
            Resolved {
                base_url: base(base_url, &key_env, ("", ""), OPENROUTER_API_URL),
                api_key,
                api_key_env: key_env,
                ..template("openrouter", "OpenRouter", WireApi::ChatCompletions, CHAT, *effort)
            }
        }
        InstanceKind::OpenaiCompatible { base_url, api_key_env, provider, wire_api, effort } => {
            let keyed = api_key_env.as_ref().is_some_and(|k| !k.trim().is_empty());
            let (api_key, key_env) = if keyed { api(api_key_env, "") } else { (String::new(), String::new()) };
            let provider = provider.clone().filter(|p| !p.trim().is_empty()).unwrap_or_else(|| "openai-compatible".into());
            let wire = wire_api.filter(|w| RESPONSES_OR_CHAT.contains(w));
            // A server krowk knows nothing of speaks Chat Completions unless
            // its definition says otherwise: the catalog describes providers,
            // not this server.
            Resolved {
                base_url: clean_url(base_url),
                api_key,
                api_key_env: key_env,
                auth: if keyed { Auth::ApiKey } else { Auth::Keyless },
                wire_pinned: true,
                ..template(&provider, "the server", wire.unwrap_or(WireApi::ChatCompletions), RESPONSES_OR_CHAT, *effort)
            }
        }
        InstanceKind::XaiOauth { base_url, issuer, client_id, scope, effort } => Resolved {
            base_url: base(base_url, "", ("", ""), XAI_API_URL),
            auth: Auth::OAuth {
                issuer: clean_url(issuer.as_deref().filter(|i| !i.trim().is_empty()).unwrap_or(XAI_ISSUER)),
                client_id: client_id.clone().filter(|c| !c.trim().is_empty()),
                scope: scope.clone().filter(|s| !s.trim().is_empty()).unwrap_or_else(|| "openid offline_access".into()),
            },
            ..template("xai", "xAI", WireApi::ChatCompletions, CHAT, *effort)
        },
        // Priced and described as Anthropic's models, which it runs; its
        // login is Claude Code's own.
        InstanceKind::ClaudeCode { binary, config_dir: dir, env: extra, args, effort } => {
            let config_dir = dir.as_deref().filter(|d| !d.trim().is_empty()).map(PathBuf::from);
            // Claude Code's own rule for its directory: CLAUDE_CONFIG_DIR —
            // the instance's, else the environment's — else ~/.claude.
            let home = config_dir.clone().or_else(|| extra.get("CLAUDE_CONFIG_DIR").or(Some(&env("CLAUDE_CONFIG_DIR"))).filter(|d| !d.trim().is_empty()).map(PathBuf::from)).or_else(|| {
                let h = env("HOME");
                (!h.trim().is_empty()).then(|| PathBuf::from(h).join(".claude"))
            });
            let binary = binary.clone().filter(|b| !b.trim().is_empty()).unwrap_or_else(|| crate::claude::BINARY.into());
            Resolved {
                auth: Auth::Vendor,
                backend: Some(Backend {
                    path: find_binary(&binary, &env("PATH")),
                    binary,
                    config_dir,
                    home,
                    env: extra.clone(),
                    args: args.clone(),
                }),
                ..template("anthropic", "Claude Code", WireApi::ClaudeCode, CLAUDE_CODE, *effort)
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

    fn env(k: &str) -> String {
        match k {
            "ANTHROPIC_API_KEY" => "sk-test".into(),
            "ANTHROPIC_BASE_URL" => "http://127.0.0.1:9/".into(),
            "OPENAI_API_KEY" => "sk-openai".into(),
            "OPENAI_BASE_URL" => "http://127.0.0.1:8/v1/".into(),
            "WORK_KEY" => "sk-work".into(),
            _ => String::new(),
        }
    }

    #[test]
    fn r_inst_a_bare_config_still_has_the_anthropic_instance_and_models_parse_either_way() {
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
        assert_eq!(reg.parse_model("router/some/model").unwrap(), ModelRef { instance: "anthropic".into(), model: "router/some/model".into() });
        assert!(reg.parse_model("anthropic/").is_err());
        assert!(reg.get("nope").unwrap_err().contains("anthropic, anthropic:work, claude, openai"));
        assert_eq!(reg.default_model().unwrap().model, DEFAULT_MODEL);
        assert!(from_config_json(&serde_json::json!({"instances": {"x": {"kind": "martian"}}})).is_err());
        // R-TOOL-2: config can pin a toolset, and only one that exists.
        assert_eq!(Registry::resolve(&from_config_json(&serde_json::json!({"toolset": "grok"})).unwrap(), &env).toolset.as_deref(), Some("grok"));
        assert!(from_config_json(&serde_json::json!({"toolset": "vim"})).unwrap_err().contains("claude, gpt, grok"));
    }

    #[test]
    fn r_prov_4_every_native_provider_has_an_instance_keyed_from_the_environment_or_a_login() {
        let cfg = from_config_json(&serde_json::json!({"instances": {
            "openai:work": {"kind": "openai-api", "apiKeyEnv": "WORK_KEY", "baseUrl": "https://gw.example/v1/", "effort": "high"},
            "local": {"kind": "openai-compatible", "baseUrl": "http://127.0.0.1:11434/v1"},
            "vivgrid": {"kind": "openai-compatible", "baseUrl": "https://api.vivgrid.com/v1", "apiKeyEnv": "WORK_KEY", "provider": "vivgrid"},
            "grok:team": {"kind": "xai-oauth", "clientId": "krowk-test", "issuer": "http://127.0.0.1:7/"},
            "pinned": {"kind": "openai-api", "wireApi": "chat-completions"},
        }}))
        .unwrap();
        let reg = Registry::resolve(&cfg, &env);
        let o = reg.get("openai").unwrap();
        assert_eq!((o.provider.as_str(), o.api_key.as_str(), o.base_url.as_str(), o.wire_api), ("openai", "sk-openai", "http://127.0.0.1:8/v1", WireApi::OpenaiResponses));
        let w = reg.get("openai:work").unwrap();
        assert_eq!((w.api_key.as_str(), w.base_url.as_str(), w.effort), ("sk-work", "https://gw.example/v1", Some(Effort::High)), "OPENAI_BASE_URL is the default instance's only");
        let x = reg.get("xai").unwrap();
        assert_eq!((x.provider.as_str(), x.api_key_env.as_str(), x.base_url.as_str(), x.wire_api), ("xai", "XAI_API_KEY", XAI_API_URL, WireApi::ChatCompletions));
        assert!(x.missing_key().unwrap().contains("set XAI_API_KEY"));
        assert_eq!(reg.get("openrouter").unwrap().base_url, OPENROUTER_API_URL);
        let l = reg.get("local").unwrap();
        assert_eq!((l.auth.clone(), l.provider.as_str(), l.missing_key()), (Auth::Keyless, "openai-compatible", None));
        assert_eq!(reg.get("vivgrid").unwrap().provider, "vivgrid");
        let g = reg.get("grok:team").unwrap();
        assert_eq!(g.auth, Auth::OAuth { issuer: "http://127.0.0.1:7".into(), client_id: Some("krowk-test".into()), scope: "openid offline_access".into() });
        assert_eq!(g.missing_key(), None, "an OAuth instance's credential is the login's to check");
        assert_eq!(reg.get("supergrok").unwrap().auth, Auth::OAuth { issuer: XAI_ISSUER.into(), client_id: None, scope: "openid offline_access".into() });
        // The catalog picks the wire API among the ones the instance speaks,
        // unless the definition pins one.
        assert_eq!(o.wire_for(Some(WireApi::ChatCompletions)), WireApi::ChatCompletions);
        assert_eq!(o.wire_for(Some(WireApi::AnthropicMessages)), WireApi::OpenaiResponses);
        assert_eq!(o.wire_for(None), WireApi::OpenaiResponses);
        assert_eq!(x.wire_for(Some(WireApi::OpenaiResponses)), WireApi::ChatCompletions);
        assert_eq!(reg.get("pinned").unwrap().wire_for(Some(WireApi::OpenaiResponses)), WireApi::ChatCompletions);
        // A compatible server is Chat Completions whatever a catalog entry
        // says, unless its definition names the Responses API.
        assert_eq!(reg.get("vivgrid").unwrap().wire_for(Some(WireApi::OpenaiResponses)), WireApi::ChatCompletions);
        let responses = Registry::resolve(&from_config_json(&serde_json::json!({"instances": {"r": {"kind": "openai-compatible", "baseUrl": "http://x/v1", "wireApi": "openai-responses"}}})).unwrap(), &env);
        assert_eq!(responses.get("r").unwrap().wire_for(Some(WireApi::ChatCompletions)), WireApi::OpenaiResponses);
        // Models: an instance by name, a bare id by its family.
        assert_eq!(reg.parse_model("openai/gpt-5.4").unwrap(), ModelRef { instance: "openai".into(), model: "gpt-5.4".into() });
        assert_eq!(reg.parse_model("openrouter/x-ai/grok-4").unwrap(), ModelRef { instance: "openrouter".into(), model: "x-ai/grok-4".into() });
        assert_eq!(reg.parse_model("gpt-5.4").unwrap().instance, "openai");
        assert_eq!(reg.parse_model("o3").unwrap().instance, "openai");
        assert_eq!(reg.parse_model("grok-4.7").unwrap().instance, "xai");
        assert_eq!(reg.parse_model("supergrok/grok-4.7").unwrap().instance, "supergrok");
        assert!(from_config_json(&serde_json::json!({"instances": {"x": {"kind": "openai-compatible"}}})).is_err(), "a compatible server needs its base URL");
    }

    #[test]
    fn r_inst_1_a_claude_instance_is_a_binary_a_config_directory_an_environment_and_arguments() {
        let env = |k: &str| match k {
            "HOME" => "/home/p".to_string(),
            "PATH" => "/nowhere".to_string(),
            _ => String::new(),
        };
        let cfg = from_config_json(&serde_json::json!({"instances": {
            "claude:work": {"kind": "claude-code", "configDir": "/cfg/work", "env": {"ANTHROPIC_BASE_URL": "https://router.example"}, "args": ["--add-dir", "/x"]},
            "claude:mine": {"kind": "claude-code", "binary": "/opt/claude/bin/claude"},
        }}))
        .unwrap();
        let reg = Registry::resolve(&cfg, &env);
        let d = reg.get("claude").unwrap();
        let b = d.backend.as_ref().unwrap();
        assert_eq!((d.kind, d.provider.as_str(), d.wire_api, d.auth.clone()), ("claude-code", "anthropic", WireApi::ClaudeCode, Auth::Vendor));
        assert_eq!((b.binary.as_str(), b.config_dir.as_deref(), b.home.as_deref()), ("claude", None, Some(std::path::Path::new("/home/p/.claude"))), "the default account is Claude Code's own");
        assert_eq!(b.path, None, "not on this PATH");
        assert_eq!(d.missing_key(), None, "its login is Claude Code's to check");
        let w = reg.get("claude:work").unwrap().backend.clone().unwrap();
        assert_eq!((w.config_dir.as_deref(), w.home.as_deref()), (Some(std::path::Path::new("/cfg/work")), Some(std::path::Path::new("/cfg/work"))));
        assert_eq!((w.env["ANTHROPIC_BASE_URL"].as_str(), w.args.clone()), ("https://router.example", vec!["--add-dir".to_string(), "/x".to_string()]), "a router is the same mechanism");
        assert_eq!(reg.get("claude:mine").unwrap().backend.as_ref().unwrap().binary, "/opt/claude/bin/claude");
        assert_eq!(reg.parse_model("claude:work/sonnet").unwrap(), ModelRef { instance: "claude:work".into(), model: "sonnet".into() });
        assert_eq!(reg.parse_model("claude/haiku").unwrap().instance, "claude");
        assert_eq!(reg.parse_model("claude-sonnet-4-6").unwrap().instance, "anthropic", "a bare Claude id is still the API's");
        let sh = find_binary("sh", &std::env::var("PATH").unwrap_or_default());
        assert!(sh.is_some_and(|p| p.is_absolute()), "a name is found on PATH");
    }

}
