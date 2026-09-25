//! The native loop: krowk's own prompt, tools and loop over a `ModelClient`.
//! One turn is model call, tool calls, model call, … until a call asks for
//! no tool, the turn is interrupted, or the step cap is hit.

use crate::engine::{BoxFuture, Engine, EngineError, EngineEvent, Events, HistoryItem, TurnContext, TurnEnd};
use crate::protocol::{Item, ItemKind, ToolDefinition, Usage, WireApi};
use crate::tools;
use tokio::sync::watch;

/// Model calls in one turn, at most: a loop that never stops asking for
/// tools is a bug or a runaway, and either should end the turn.
const MAX_STEPS: usize = 200;

/// One model call, provider-neutral.
#[derive(Debug, Clone)]
pub struct ModelRequest {
    pub model: String,
    pub system: String,
    pub tools: Vec<ToolDefinition>,
    pub history: Vec<HistoryItem>,
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
    fn stream<'a>(&'a self, req: &'a ModelRequest, events: &'a Events, cancel: watch::Receiver<bool>) -> BoxFuture<'a, Result<ModelResponse, EngineError>>;
}

pub struct NativeEngine<C: ModelClient> {
    pub client: C,
}

/// The system prompt: small and identical from call to call, because it is
/// the front of the cached prefix. Nothing volatile goes in it — no date,
/// no clock, nothing a second call would render differently.
pub fn system_prompt(cwd: &std::path::Path) -> String {
    format!(
        "You are krowk, a coding agent working in a terminal on the user's machine.\n\
         The working directory is {}. Relative paths resolve against it.\n\
         Use the tools to look before you answer: read files rather than guessing what they hold.\n\
         Be direct and brief. When the task is done, say what you found or did in plain text.",
        cwd.display()
    )
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
            let system = system_prompt(&ctx.cwd);
            let tool_defs = tools::definitions();
            let _ = events.send(EngineEvent::Context { system: system.clone(), tools: tool_defs.clone() }).await;
            let mut req = ModelRequest { model: ctx.model.model.clone(), system, tools: tool_defs, history: ctx.history.clone() };
            // Response indexes continue from the history's, so a replayed
            // turn and this one never share an index.
            let first_response = req.history.iter().filter_map(|h| h.response).max().map_or(0, |m| m + 1);
            let tool_env = tools::ToolEnv { cwd: &ctx.cwd, permission_mode: ctx.permission_mode };
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
