package mcp

import (
	"slices"
	"strings"
)

// ToolSurface is one tool as an agent sees it over the wire: what it is called
// and what it takes. Not the descriptions — those are prose, they change often,
// and pinning them would turn a copy edit into a contract change.
//
// It exists so the CLI's surface snapshot can carry the MCP tools alongside the
// commands. The two are one product: an agent reaches for `krowk_push` or for
// `krowk push` depending on whether it can shell out, and a tool that quietly
// loses an argument is the same broken promise as a flag that does.
type ToolSurface struct {
	Name string `json:"name"`
	// Required names the arguments a call must carry.
	Required []string `json:"required,omitempty"`
	// Arguments is every argument the tool accepts, required ones included.
	Arguments []string `json:"arguments,omitempty"`
}

// Surface reports the tools this server registers, derived from the same
// schemas it answers tools/list with, so the snapshot cannot describe a server
// that is not this one.
//
// Sorted throughout: the schemas hold their properties in a map, and an
// unsorted snapshot would fail at random.
func Surface() []ToolSurface {
	schemas := toolSchemas()
	surface := make([]ToolSurface, 0, len(schemas))

	for _, schema := range schemas {
		name, _ := schema["name"].(string)
		tool := ToolSurface{Name: name}

		input, _ := schema["inputSchema"].(map[string]any)
		if required, ok := input["required"].([]string); ok {
			tool.Required = slices.Sorted(slices.Values(required))
		}
		if properties, ok := input["properties"].(map[string]any); ok {
			for argument := range properties {
				tool.Arguments = append(tool.Arguments, argument)
			}
			slices.Sort(tool.Arguments)
		}
		surface = append(surface, tool)
	}

	slices.SortFunc(surface, func(a, b ToolSurface) int {
		return strings.Compare(a.Name, b.Name)
	})
	return surface
}
