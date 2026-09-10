package importer

import (
	"encoding/json"
	"testing"
)

// Acceptance: every part.type a Source emits is in the fixed set, and an
// unknown source type maps to `unknown` with the raw payload in data.
func TestNormalizePartMapsOntoTheFixedSet(t *testing.T) {
	tests := []struct {
		name    string
		rawType string
		raw     string
		want    string
		known   bool
	}{
		{name: "text", rawType: PartText, raw: `{"text":"hi"}`, want: PartText, known: true},
		{name: "thinking", rawType: PartThinking, raw: `{"thinking":"hm"}`, want: PartThinking, known: true},
		{name: "tool_call", rawType: PartToolCall, raw: `{"name":"sh"}`, want: PartToolCall, known: true},
		{name: "tool_result", rawType: PartToolResult, raw: `{"output":"ok"}`, want: PartToolResult, known: true},
		{name: "image", rawType: PartImage, raw: `{}`, want: PartImage, known: true},
		{name: "file", rawType: PartFile, raw: `{}`, want: PartFile, known: true},
		{name: "patch", rawType: PartPatch, raw: `{}`, want: PartPatch, known: true},
		{name: "step", rawType: PartStep, raw: `{}`, want: PartStep, known: true},
		{name: "a type nobody has seen", rawType: "redacted_reasoning", raw: `{"x":1}`, want: PartUnknown},
		{name: "empty type", rawType: "", raw: `{"x":1}`, want: PartUnknown},
		// Normalizing twice must not nest: a part already marked unknown
		// passes through as itself, so a second pass neither re-wraps the
		// payload nor reports the same missing type again.
		{name: "unknown passes through untouched", rawType: PartUnknown, raw: `{"source_type":"x","raw":{}}`, want: PartUnknown, known: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part, ok := NormalizePart(tt.rawType, json.RawMessage(tt.raw))
			if part.Type != tt.want {
				t.Fatalf("Type = %q, want %q", part.Type, tt.want)
			}
			if ok != tt.known {
				t.Fatalf("ok = %v, want %v", ok, tt.known)
			}
			if !KnownPartType(part.Type) {
				t.Fatalf("Type %q is outside the fixed set", part.Type)
			}
			if part.Data != "" && !json.Valid([]byte(part.Data)) {
				t.Fatalf("Data is not valid JSON: %q", part.Data)
			}
			if tt.want != PartUnknown || tt.known {
				return
			}
			// The unknown case has to keep both the raw payload and the
			// type it came in as, or the row cannot say unknown-what.
			var got struct {
				SourceType string          `json:"source_type"`
				Raw        json.RawMessage `json:"raw"`
			}
			if err := json.Unmarshal([]byte(part.Data), &got); err != nil {
				t.Fatalf("unmarshal unknown data: %v", err)
			}
			if got.SourceType != tt.rawType {
				t.Fatalf("source_type = %q, want %q", got.SourceType, tt.rawType)
			}
			if string(got.Raw) != tt.raw {
				t.Fatalf("raw = %s, want %s", got.Raw, tt.raw)
			}
		})
	}
}

// Acceptance (counting half): an unknown type is counted, with the raw type
// kept so the gap is actionable.
func TestResultNormalizePartCountsUnknown(t *testing.T) {
	var res Result
	res.NormalizePart(PartText, json.RawMessage(`{"text":"hi"}`))
	first := res.NormalizePart("server_tool_use", json.RawMessage(`{}`))
	res.NormalizePart("server_tool_use", json.RawMessage(`{}`))
	if res.Unknown != 2 {
		t.Fatalf("Unknown = %d, want 2", res.Unknown)
	}
	// Re-normalizing an already-unknown part counts nothing further and
	// changes nothing: the operation is idempotent.
	again := res.NormalizePart(first.Type, json.RawMessage(first.Data))
	if again.Type != PartUnknown || again.Data != first.Data {
		t.Fatalf("re-normalized = %+v, want %+v unchanged", again, first)
	}
	if res.Unknown != 2 {
		t.Fatalf("Unknown = %d after re-normalizing, want 2", res.Unknown)
	}
	if res.UnknownTypes["server_tool_use"] != 2 {
		t.Fatalf("UnknownTypes = %v", res.UnknownTypes)
	}
}

func TestKnownPartTypeAndPartTypesAgree(t *testing.T) {
	types := PartTypes()
	if len(types) != 9 {
		t.Fatalf("PartTypes has %d entries, want the nine fixed types", len(types))
	}
	for _, ty := range types {
		if !KnownPartType(ty) {
			t.Fatalf("PartTypes lists %q but KnownPartType refuses it", ty)
		}
	}
	for _, ty := range []string{"reasoning", "tool-call", "TEXT", ""} {
		if KnownPartType(ty) {
			t.Fatalf("KnownPartType(%q) = true, want false", ty)
		}
	}
}

func TestToolCallAndResultDataShapes(t *testing.T) {
	call := NewToolCallPart("call_1", "Bash", json.RawMessage(`{"command":"ls"}`))
	if call.Type != PartToolCall || call.ToolCallID != "call_1" {
		t.Fatalf("call = %+v", call)
	}
	var cd ToolCallData
	if err := json.Unmarshal([]byte(call.Data), &cd); err != nil {
		t.Fatalf("unmarshal call data: %v", err)
	}
	if cd.Name != "Bash" || string(cd.Input) != `{"command":"ls"}` {
		t.Fatalf("call data = %+v", cd)
	}

	res := NewToolResultPart("call_1", json.RawMessage(`{"stdout":"a"}`), true)
	if res.Type != PartToolResult || res.ToolCallID != "call_1" {
		t.Fatalf("result = %+v", res)
	}
	var rd ToolResultData
	if err := json.Unmarshal([]byte(res.Data), &rd); err != nil {
		t.Fatalf("unmarshal result data: %v", err)
	}
	if string(rd.Output) != `{"stdout":"a"}` || !rd.IsError {
		t.Fatalf("result data = %+v", rd)
	}

	// is_error is written even when false: "succeeded" must be tellable
	// from "nobody recorded it".
	if got := NewToolResultTextPart("call_2", "ok", false).Data; got != `{"output":"ok","is_error":false}` {
		t.Fatalf("text result data = %s", got)
	}

	// A missing input is JSON null, never invalid JSON the store would
	// reject at ingest time.
	if got := NewToolCallPart("call_3", "Read", nil).Data; got != `{"name":"Read","input":null}` {
		t.Fatalf("nil-input call data = %s", got)
	}

	// An input that is not valid JSON is kept as a JSON string rather than
	// replaced with null: the input is documented as verbatim, and null
	// would discard the only record of what was actually run.
	got := NewToolCallPart("call_4", "Bash", json.RawMessage(`{"command": ls`)).Data
	if got != `{"name":"Bash","input":"{\"command\": ls"}` {
		t.Fatalf("invalid-input call data = %s", got)
	}
	var kept ToolCallData
	if err := json.Unmarshal([]byte(got), &kept); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var raw string
	if err := json.Unmarshal(kept.Input, &raw); err != nil || raw != `{"command": ls` {
		t.Fatalf("input round-trip = %q, %v", raw, err)
	}
}

// Data always has to be something the store will accept, so a payload that
// is not JSON is wrapped rather than passed through to fail later.
func TestNormalizePartWrapsInvalidJSONPayload(t *testing.T) {
	part, ok := NormalizePart(PartText, json.RawMessage(`not json`))
	if !ok || part.Type != PartText {
		t.Fatalf("part = %+v ok = %v", part, ok)
	}
	if !json.Valid([]byte(part.Data)) {
		t.Fatalf("Data = %q, want valid JSON", part.Data)
	}
	if part.Data != `{"raw":"not json"}` {
		t.Fatalf("Data = %q", part.Data)
	}
}
