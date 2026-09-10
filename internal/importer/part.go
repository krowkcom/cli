package importer

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/krowkcom/cli/internal/store"
)

// The canonical part types. store.Part.Type is an open string at the schema
// so a new content block never needs a migration; it is closed here so the
// three sources cannot spell the same block three ways. A reader downstream
// asking "was this a tool call" must not have to know which agent wrote the
// row.
//
// The set is small on purpose. Every provider-specific nuance that does not
// change what a reader does with the block belongs in Data, not in a new
// type — a redacted thinking block is still PartThinking, and a
// server-executed tool is still PartToolCall.
const (
	// PartText is prose, the message a person reads.
	PartText = "text"
	// PartThinking is reasoning the model emitted separately from its
	// answer, including the redacted and signed variants.
	PartThinking = "thinking"
	// PartToolCall is a request to run a tool. Data is ToolCallData; the
	// part carries the call id.
	PartToolCall = "tool_call"
	// PartToolResult is what a tool returned. Data is ToolResultData; the
	// part carries the same call id as its PartToolCall.
	PartToolResult = "tool_result"
	// PartImage is an image, whether attached by the user or produced by a
	// tool.
	PartImage = "image"
	// PartFile is a non-image attachment or file reference.
	PartFile = "file"
	// PartPatch is a diff — an edit proposed or applied, kept apart from
	// text because it is the one content kind a reader will want to render
	// as a diff rather than as prose.
	PartPatch = "patch"
	// PartStep is an agent step or phase marker: opencode's step-start and
	// its kin, which delimit work rather than carry it.
	PartStep = "step"
	// PartUnknown is a block this build did not recognise. It is not an
	// error and not a drop: Data holds the raw payload, so the transcript
	// stays complete and the missing case is visible in Result.Unknown
	// instead of being discovered later as a gap.
	PartUnknown = "unknown"
)

// knownPartTypes is the set, in one place, so KnownPartType and the test
// that pins the constants cannot disagree.
var knownPartTypes = map[string]bool{
	PartText:       true,
	PartThinking:   true,
	PartToolCall:   true,
	PartToolResult: true,
	PartImage:      true,
	PartFile:       true,
	PartPatch:      true,
	PartStep:       true,
	PartUnknown:    true,
}

// KnownPartType reports whether t is a canonical part type. A source must
// emit nothing else; NormalizePart is how it guarantees that without having
// to enumerate its own vocabulary at every call site.
func KnownPartType(t string) bool { return knownPartTypes[t] }

// PartTypes is the canonical set as a slice, for callers that need to
// enumerate it (documentation, a doctor listing, the test that pins it).
func PartTypes() []string {
	return []string{
		PartText, PartThinking, PartToolCall, PartToolResult,
		PartImage, PartFile, PartPatch, PartStep, PartUnknown,
	}
}

// ToolCallData is the fixed shape of a PartToolCall's Data. Name is the tool
// as the provider named it — unmapped, because a tool called `Bash` in one
// agent and `bash` in another are genuinely different tools with different
// input schemas. Input is whatever the provider sent, unchanged when it is
// valid UTF-8 JSON; see normalizeRaw for what happens when it is not, which
// is the one case where the bytes are not preserved exactly.
type ToolCallData struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolResultData is the fixed shape of a PartToolResult's Data. Output is
// the raw result — a string for most tools, structured JSON for some, so it
// stays a raw message rather than being flattened into text. IsError is
// always written, including when false: a reader must be able to tell "the
// tool succeeded" from "nobody recorded whether it did".
type ToolResultData struct {
	Output  json.RawMessage `json:"output"`
	IsError bool            `json:"is_error"`
}

// NewToolCallPart builds the tool_call part for a call, so the shape is
// produced in one place rather than assembled from a format string in three
// importers. A nil or empty input becomes JSON null, never invalid JSON.
func NewToolCallPart(callID, name string, input json.RawMessage) store.Part {
	return store.Part{
		Type:       PartToolCall,
		ToolCallID: callID,
		Data:       mustJSON(ToolCallData{Name: name, Input: normalizeRaw(input)}),
	}
}

// NewToolResultPart builds the tool_result part for a call's output, keyed
// by the same call id its tool_call carried — that pairing is the only way a
// reader reassembles the two, since nothing in the schema joins them.
func NewToolResultPart(callID string, output json.RawMessage, isError bool) store.Part {
	return store.Part{
		Type:       PartToolResult,
		ToolCallID: callID,
		Data:       mustJSON(ToolResultData{Output: normalizeRaw(output), IsError: isError}),
	}
}

// NewToolResultTextPart is NewToolResultPart for the common case of a tool
// that returned a plain string, saving every caller a json.Marshal of a
// string.
func NewToolResultTextPart(callID, output string, isError bool) store.Part {
	return NewToolResultPart(callID, json.RawMessage(mustJSON(output)), isError)
}

// NormalizePart maps a source's own block type onto the canonical set. A
// recognised type keeps its raw payload as Data; an unrecognised one becomes
// PartUnknown with the same payload, because the alternative — dropping it —
// turns a transcript krowk could not fully parse into a transcript that
// looks complete and is not.
//
// ok reports whether rawType was canonical, which is what a caller counts.
// Sources that need the counting done for them should use
// Result.NormalizePart instead.
// Normalizing is idempotent, which matters because a part can pass through
// here twice — once as a source read it, once as a caller re-wrapped it.
// PartUnknown in is PartUnknown out, with the payload untouched and nothing
// counted again: a second pass must not nest an unknown inside an unknown
// and report a second missing type that was already reported.
func NormalizePart(rawType string, raw json.RawMessage) (part store.Part, ok bool) {
	if KnownPartType(rawType) {
		return store.Part{Type: rawType, Data: rawDataOrEmpty(raw)}, true
	}
	// The raw type would otherwise be lost: Type is now `unknown`, and
	// without this the row would not say unknown-what.
	return store.Part{Type: PartUnknown, Data: unknownData(rawType, raw)}, false
}

// NormalizePart is NormalizePart with the accounting attached, so a source
// cannot normalise a block and forget to count it.
func (r *Result) NormalizePart(rawType string, raw json.RawMessage) store.Part {
	part, ok := NormalizePart(rawType, raw)
	if !ok {
		r.Unknown++
		if r.UnknownTypes == nil {
			r.UnknownTypes = map[string]int{}
		}
		r.UnknownTypes[rawType]++
	}
	return part
}

// unknownData is the PartUnknown payload: the source's type alongside its
// untouched block.
func unknownData(rawType string, raw json.RawMessage) string {
	return mustJSON(struct {
		SourceType string          `json:"source_type"`
		Raw        json.RawMessage `json:"raw,omitempty"`
	}{SourceType: rawType, Raw: normalizeRawOmit(raw)})
}

// rawDataOrEmpty is the part Data for a payload that needs no reshaping.
// The store defaults an empty Data to '{}', and a payload that is not usable
// JSON would fail further down and further from the cause, so it is wrapped
// rather than passed through.
func rawDataOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if !usableJSON(raw) {
		return mustJSON(struct {
			Raw string `json:"raw"`
		}{Raw: string(raw)})
	}
	return string(raw)
}

// usableJSON reports whether a payload can be stored as it stands. Valid
// JSON is not enough: JSON syntax admits arbitrary bytes inside a string
// literal, and TEXT columns, terminals and JSON consumers downstream all
// assume UTF-8. A payload that is valid JSON carrying invalid UTF-8 is
// therefore refused here and wrapped instead, where Go's marshaller
// normalises it.
func usableJSON(raw json.RawMessage) bool {
	return json.Valid(raw) && utf8.Valid(raw)
}

// normalizeRaw makes a payload safe to embed without throwing it away. An
// absent one becomes JSON null, because ToolCallData and ToolResultData must
// always marshal to valid JSON. One that is not usable as it stands — bad
// syntax, or valid syntax carrying bytes that are not UTF-8 — is kept as a
// JSON string rather than replaced with null: discarding it would lose the
// only record of what was actually run.
//
// "Kept" is not quite "byte for byte", and that is the one place this
// package does not preserve what it read. Marshalling the bytes as a Go
// string replaces each invalid UTF-8 sequence with U+FFFD, so a payload
// carrying raw binary comes back readable but not identical. The
// alternative was storing bytes that no reader of the column could decode.
func normalizeRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	if !usableJSON(raw) {
		return json.RawMessage(mustJSON(string(raw)))
	}
	return raw
}

// normalizeRawOmit is normalizeRaw for an omitempty field, where absent
// means absent rather than null.
func normalizeRawOmit(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return normalizeRaw(raw)
}

// mustJSON marshals a shape defined in this package. The shapes hold only
// strings, bools and pre-validated raw JSON, so a failure here is not a bad
// transcript, it is a bug in this file — and returning '{}' keeps the store
// contract (Data is always valid JSON) rather than propagating an error no
// caller could act on.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
