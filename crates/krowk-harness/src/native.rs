//! The native loop: krowk's own prompt, tools and loop over a `ModelClient`.
//! One turn is model call, tool calls, model call, … until a call asks for
//! no tool, the turn is interrupted, or the step cap is hit.

use crate::engine::{BoxFuture, Engine, EngineError, EngineEvent, Events, HistoryItem, TurnContext, TurnEnd};
use crate::protocol::{Effort, Item, ItemKind, ProviderBlob, ToolDefinition, Usage, WireApi};
use crate::tools;
use crate::toolset::Toolset;
use tokio::sync::watch;

/// Model calls in one turn, at most: a loop that never stops asking for
/// tools is a bug or a runaway, and either should end the turn.
const MAX_STEPS: usize = 200;

/// One model call, provider-neutral.
#[derive(Debug, Clone, Default)]
pub struct ModelRequest {
    pub model: String,
    pub system: String,
    pub tools: Vec<ToolDefinition>,
    pub history: Vec<HistoryItem>,
    /// The session's id. It keys the provider's prompt cache where the
    /// provider takes a key (OpenAI's `prompt_cache_key`, xAI's
    /// conversation id), so every call of a session lands where its prefix
    /// is cached (R-PROV-3).
    pub session_id: String,
    /// The effort the model is sent, already mapped onto the rungs it takes;
    /// none sends nothing, and the provider's default applies.
    pub effort: Option<Effort>,
    /// The model reasons: the catalog's word, else its family's.
    pub reasoning: bool,
}

/// Whether a reasoning blob replays to this client: its own provider and
/// wire API only (R-LOG-3), decided by the blob and never by where its item
/// sits.
pub fn replays<'a>(blob: &'a Option<ProviderBlob>, provider: &str, wire: WireApi) -> Option<&'a ProviderBlob> {
    blob.as_ref().filter(|b| b.provider == provider && b.wire_api == wire)
}

/// Reasoning another provider (or another wire API) produced, as it
/// crosses to this one (R-SWITCH-1): its blob cannot be read here, so it is
/// downgraded to plain text — the loss the spec accepts. The text goes back
/// inside the assistant message it belonged to, framed as reasoning from an
/// earlier model, so it is never passed off as something this model said.
/// Reasoning with no readable text (encrypted or redacted only) has nothing
/// to downgrade and is left out.
pub fn downgraded(text: &str) -> Option<String> {
    let text = text.trim();
    (!text.is_empty()).then(|| format!("<reasoning from an earlier model>\n{text}\n</reasoning>"))
}

/// What one call produced.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct ModelResponse {
    pub response_id: Option<String>,
    pub model: String,
    pub usage: Usage,
    pub stop_reason: Option<String>,
    /// The completed items, in order, each under the id its live frames used.
    pub items: Vec<(String, Item)>,
    /// The call was cut short by an interrupt.
    pub interrupted: bool,
}

/// A provider's wire API. It streams the call's items as `ItemStarted`,
/// `ItemDelta` and `ItemCompleted` on `events` as they arrive, and returns
/// them all at the end; the loop adds the `ResponseCompleted`.
pub trait ModelClient: Send + Sync {
    fn provider(&self) -> &str;
    fn wire_api(&self) -> WireApi;
    /// Whether this model takes freeform (grammar) tools, so `apply_patch`
    /// is offered as one. The Messages API has none.
    fn custom_tools(&self, _model: &str) -> bool {
        false
    }
    fn stream<'a>(&'a self, req: &'a ModelRequest, events: &'a Events, cancel: watch::Receiver<bool>) -> BoxFuture<'a, Result<ModelResponse, EngineError>>;
}

pub struct NativeEngine<C: ModelClient> {
    pub client: C,
}

/// The system prompt: small and identical from call to call, because it is
/// the front of the cached prefix. Nothing volatile goes in it — no date,
/// no clock, nothing a second call would render differently.
/// It names the turn's edit tool, which is fixed for the session's model.
pub fn system_prompt(cwd: &std::path::Path, toolset: &Toolset) -> String {
    format!(
        "You are krowk, a coding agent working in a terminal on the user's machine.\n\
         The working directory is {}. Relative paths resolve against it.\n\
         Use the tools to look before you answer: read files rather than guessing what they hold, and find them with grep and glob.\n\
         Change existing files with {}; create new ones with write.\n\
         Be direct and brief. When the task is done, say what you found or did in plain text.",
        cwd.display(),
        toolset.preset.edit.name()
    )
}

/// Tokens in a text, estimated at four bytes a token — the ratio providers
/// quote for English and code. No tokenizer ships with krowk; what the
/// estimate is for is noticing growth, which it does whatever the true
/// ratio is.
pub fn estimate_tokens(text: &str) -> u64 {
    (text.len() as u64).div_ceil(4)
}

/// The tool definitions' estimated tokens, over the JSON a provider is sent
/// for them.
pub fn tools_tokens(tools: &[ToolDefinition]) -> u64 {
    tools.iter().map(|t| estimate_tokens(&serde_json::to_string(t).expect("a definition serializes"))).sum()
}

impl<C: ModelClient> Engine for NativeEngine<C> {
    fn provider(&self) -> &str {
        self.client.provider()
    }

    fn wire_api(&self) -> WireApi {
        self.client.wire_api()
    }

    fn run_turn<'a>(&'a self, ctx: TurnContext, events: Events) -> BoxFuture<'a, Result<TurnEnd, EngineError>> {
        Box::pin(async move {
            let toolset = Toolset { preset: ctx.preset, custom_tools: self.client.custom_tools(&ctx.model.model) };
            let system = system_prompt(&ctx.cwd, &toolset);
            let tool_defs = tools::definitions(&toolset);
            let _ = events.send(EngineEvent::Context { system: system.clone(), tools: tool_defs.clone() }).await;
            let family = ctx.model_info.as_ref().and_then(|i| i.family.clone()).or_else(|| crate::toolset::family_from_id(&ctx.model.model).map(String::from));
            let takes = crate::effort::supported(ctx.model_info.as_ref(), family.as_deref());
            let mut req = ModelRequest {
                model: ctx.model.model.clone(),
                system,
                tools: tool_defs,
                history: ctx.history.clone(),
                session_id: ctx.session_id.clone(),
                effort: ctx.effort.and_then(|e| crate::effort::map(e, &takes)),
                reasoning: ctx.model_info.as_ref().map_or_else(|| crate::toolset::reasons(&ctx.model.model), |i| i.reasoning),
            };
            // Response indexes continue from the history's, so a replayed
            // turn and this one never share an index.
            let first_response = req.history.iter().filter_map(|h| h.response).max().map_or(0, |m| m + 1);
            let tool_env = tools::ToolEnv { cwd: &ctx.cwd, permission_mode: ctx.permission_mode, edit: ctx.preset.edit };
            for response in (first_response..).take(MAX_STEPS) {
                if *ctx.cancel.borrow() {
                    return Ok(TurnEnd::Interrupted);
                }
                let resp = self.client.stream(&req, &events, ctx.cancel.clone()).await?;
                let item_ids: Vec<String> = resp.items.iter().map(|(id, _)| id.clone()).collect();
                let _ = events
                    .send(EngineEvent::ResponseCompleted {
                        response_id: resp.response_id.clone(),
                        model: resp.model.clone(),
                        usage: resp.usage,
                        stop_reason: resp.stop_reason.clone(),
                        item_ids,
                    })
                    .await;
                let response = Some(response);
                let calls: Vec<(String, String, serde_json::Value)> = resp
                    .items
                    .iter()
                    .filter_map(|(_, it)| match it {
                        Item::ToolCall { call_id, name, input } => Some((call_id.clone(), name.clone(), input.clone())),
                        _ => None,
                    })
                    .collect();
                req.history.extend(resp.items.into_iter().map(|(_, item)| HistoryItem { item, response }));
                if resp.interrupted {
                    return Ok(TurnEnd::Interrupted);
                }
                if calls.is_empty() {
                    return Ok(TurnEnd::Completed);
                }
                let mut interrupted = false;
                for (call_id, name, input) in calls {
                    let item_id = krowk_store::new_id();
                    let _ = events.send(EngineEvent::ItemStarted { item_id: item_id.clone(), kind: ItemKind::ToolResult { call_id: call_id.clone() } }).await;
                    // Every call gets its result, even one never run: a call
                    // with no result cannot be sent back to the provider.
                    let (output, is_error) = if interrupted {
                        ("not run: the turn was interrupted".to_string(), true)
                    } else {
                        let mut cancel = ctx.cancel.clone();
                        tokio::select! {
                            r = tools::run(&name, &input, &tool_env) => r,
                            _ = crate::engine::cancelled(&mut cancel) => {
                                interrupted = true;
                                ("interrupted before it finished".to_string(), true)
                            }
                        }
                    };
                    let item = Item::ToolResult { call_id, output, is_error };
                    let _ = events.send(EngineEvent::ItemCompleted { item_id, item: item.clone() }).await;
                    req.history.push(HistoryItem { item, response: None });
                }
                if interrupted {
                    return Ok(TurnEnd::Interrupted);
                }
            }
            Err(EngineError::new(
                "turn_step_limit",
                format!("the turn made {MAX_STEPS} model calls without finishing, so it was stopped — ask again with a narrower task"),
            ))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::toolset::PRESETS;

    /// The ceiling on the system prompt plus tool definitions, in estimated
    /// tokens: the `context.tokens` budget in krowk-bench's budgets.toml,
    /// where `make bench` holds the built binary to it. Read from there so
    /// the two cannot disagree; this holds every preset in both tool forms,
    /// including apply_patch's freeform one no wire API reaches yet.
    fn context_tokens_budget() -> u64 {
        let file = include_str!("../../krowk-bench/budgets.toml");
        let at = file.find("id = \"context.tokens\"").expect("budgets.toml has context.tokens");
        let max = file[at..].lines().find_map(|l| l.strip_prefix("max = ")).expect("context.tokens has a max");
        max.trim().replace('_', "").parse().expect("a whole number of tokens")
    }

    #[test]
    fn r_tool_1_the_system_prompt_and_tool_definitions_stay_small() {
        // A long, realistic working directory: it is the one variable part.
        let cwd = std::path::Path::new("/home/someone/Repositories/a-project-with-a-long-name");
        let budget = context_tokens_budget();
        for preset in PRESETS {
            for custom_tools in [false, true] {
                let ts = Toolset { preset, custom_tools };
                let system = system_prompt(cwd, &ts);
                let (s, t) = (estimate_tokens(&system), tools_tokens(&tools::definitions(&ts)));
                println!("R-TOOL-1 context tokens: {} (custom tools {custom_tools}): system {s} + tools {t} = {}", preset.name, s + t);
                assert!(s < 150, "the system prompt is {s} tokens: keep it a few lines");
                assert!(s + t <= budget, "{}: system + tools is {} tokens, over the {budget} budget (context.tokens in krowk-bench/budgets.toml)", preset.name, s + t);
                assert_eq!(system, system_prompt(cwd, &ts), "nothing volatile: the prompt is the front of the cached prefix");
            }
        }
    }
}
