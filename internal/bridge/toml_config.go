package bridge

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

var codexMCPKey = []string{"mcp_servers", "slopguard"}

type span struct{ start, end int }

func tomlSnippet(command string) string {
	return "[mcp_servers.slopguard]\ncommand = " + strconv.Quote(command) + "\nargs = [\"mcp\"]\n"
}

func patchTOML(data []byte, command string) ([]byte, error) {
	var config map[string]any
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	parser := unstable.Parser{}
	parser.Reset(data)
	var table []string
	var remove []span
	inline := false
	for parser.NextExpression() {
		e := parser.Expression()
		keys := e.Key()
		keyStart, keyEnd := 0, 0
		var names []string
		for keys.Next() {
			k := keys.Node()
			if len(names) == 0 {
				keyStart = int(k.Raw.Offset)
			}
			keyEnd = int(k.Raw.Offset + k.Raw.Length)
			names = append(names, string(k.Data))
		}
		if e.Kind == unstable.Table || e.Kind == unstable.ArrayTable {
			table = names
			if hasPrefix(table, codexMCPKey) {
				start := keyStart
				for start > 0 && data[start] != '[' {
					start--
				}
				if e.Kind == unstable.ArrayTable {
					start--
				}
				end := keyEnd + bytes.IndexByte(data[keyEnd:], ']') + 1
				if e.Kind == unstable.ArrayTable {
					end++
				}
				remove = append(remove, span{start, end})
			}
			continue
		}
		full := append(slices.Clone(table), names...)
		value := e.Value()
		if hasPrefix(full, codexMCPKey) {
			remove = append(remove, span{keyStart, tomlValueEnd(data, value)})
			continue
		}
		if slices.Equal(full, codexMCPKey[:1]) {
			if value.Kind != unstable.InlineTable {
				return nil, fmt.Errorf("mcp_servers must be a TOML table")
			}
			inline = true
			data = patchInlineTOML(data, value, command)
			break
		}
	}
	if err := parser.Error(); err != nil {
		return nil, err
	}
	if !inline {
		for i := len(remove) - 1; i >= 0; i-- {
			data = splice(data, remove[i].start, remove[i].end, nil)
		}
		if command != "" {
			data = appendBlock(data, tomlSnippet(command))
		}
	}
	return data, toml.Unmarshal(data, &config)
}

func patchInlineTOML(data []byte, node *unstable.Node, command string) []byte {
	type member struct {
		span
		owned bool
	}
	var members []member
	children := node.Children()
	for children.Next() {
		entry := children.Node()
		keys := entry.Key()
		keys.Next()
		members = append(members, member{span{int(keys.Node().Raw.Offset), tomlValueEnd(data, entry.Value())}, string(keys.Node().Data) == "slopguard"})
	}
	// Inline tables cannot contain comments. Keep every unrelated member's bytes.
	start, end := int(node.Raw.Offset)+1, tomlValueEnd(data, node)-1
	var text []string
	for _, m := range members {
		if !m.owned {
			text = append(text, string(data[m.start:m.end]))
		}
	}
	if command != "" {
		text = append(text, "slopguard = {command = "+strconv.Quote(command)+", args = [\"mcp\"]}")
	}
	if len(text) == len(members) && command == "" {
		return data
	}
	// Retain the original spacing whenever no SlopGuard member needs replacing.
	if len(text) == len(members)+1 {
		at := start
		tail := text[len(text)-1]
		if len(members) > 0 {
			at = members[len(members)-1].end
			tail = ", " + tail
		}
		return splice(data, at, at, []byte(tail))
	}
	var edits []span
	for i := 0; i < len(members); {
		if !members[i].owned {
			i++
			continue
		}
		j := i + 1
		for j < len(members) && members[j].owned {
			j++
		}
		first, last := members[i].start, members[j-1].end
		if j < len(members) {
			last += bytes.IndexByte(data[last:members[j].start], ',') + 1
		} else if i > 0 {
			first = members[i-1].end + bytes.IndexByte(data[members[i-1].end:first], ',')
		}
		edits = append(edits, span{first, last})
		i = j
	}
	if command == "" {
		for i := len(edits) - 1; i >= 0; i-- {
			data = splice(data, edits[i].start, edits[i].end, nil)
		}
		return data
	}
	if len(edits) == 1 && len(text) == len(members) && !members[0].owned {
		// For a single existing member, replace just its key and value.
		for _, m := range members {
			if m.owned {
				return splice(data, m.start, m.end, []byte(text[len(text)-1]))
			}
		}
	}
	// Remove the matching entries and insert the generated entry after the last
	// remaining member, leaving unrelated entries and their spacing intact.
	at := start
	prefix := ""
	for i := len(members) - 1; i >= 0; i-- {
		if !members[i].owned {
			at = members[i].end
			prefix = ", "
			break
		}
	}
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		data = splice(data, e.start, e.end, nil)
		if e.end <= at {
			at -= e.end - e.start
		}
	}
	if at > end {
		return data
	}
	return splice(data, at, at, []byte(prefix+text[len(text)-1]))
}

func tomlValueEnd(data []byte, node *unstable.Node) int {
	end := int(node.Raw.Offset + node.Raw.Length)
	if node.Kind == unstable.InlineTable {
		it := node.Children()
		for it.Next() {
			end = tomlValueEnd(data, it.Node().Value())
		}
		end += bytes.IndexByte(data[end:], '}') + 1
	}
	return end
}

func hasPrefix(value, prefix []string) bool {
	return len(value) >= len(prefix) && slices.Equal(value[:len(prefix)], prefix)
}

func appendBlock(data []byte, block string) []byte {
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		block = "\n" + block
	}
	if bytes.Contains(data, []byte("\r\n")) {
		block = strings.ReplaceAll(block, "\n", "\r\n")
	}
	return splice(data, len(data), len(data), []byte(block))
}
