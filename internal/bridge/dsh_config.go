package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const dshRowID = "mcp-slopguard"

func dshRow(command string) string {
	return "- insert:\n" +
		"    - id: " + dshRowID + "\n" +
		"      name: '@deepseek-ai/dsh-mcp-client'\n" +
		"      config:\n" +
		"        serverName: slopguard\n" +
		"        transport: stdio\n" +
		"        command: " + strconv.Quote(command) + "\n" +
		"        args: [mcp]\n"
}

func patchDSH(data []byte, command string) ([]byte, error) {
	document, err := parseDSH(data)
	if err != nil {
		return nil, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	offsets := make([]int, len(lines))
	for i := 1; i < len(lines); i++ {
		offsets[i] = offsets[i-1] + len(lines[i-1])
	}
	if document != nil && document.Style&yaml.FlowStyle != 0 {
		if len(document.Content) != 0 {
			return patchDSHFlow(data, document, offsets, command)
		}
		offset := offsets[document.Line-1] + document.Column - 1
		end := offset + bytes.IndexByte(data[offset:], ']') + 1
		if command == "" {
			return data, nil
		}
		data = splice(data, offset, end, dshFlowPatch(command))
		_, err = parseDSH(data)
		return data, err
	}
	if document != nil {
		var edits []span
		for _, row := range document.Content {
			insert := yamlChild(row, "insert")
			if insert == nil || insert.Kind != yaml.SequenceNode {
				continue
			}
			for _, entry := range insert.Content {
				id := yamlChild(entry, "id")
				if id == nil || id.Value != dshRowID {
					continue
				}
				if len(insert.Content) == 1 && len(row.Content) == 2 {
					edits = append(edits, yamlRow(lines, offsets, row.Line-1))
				} else {
					edits = append(edits, yamlRow(lines, offsets, entry.Line-1))
				}
			}
		}
		for i := len(edits) - 1; i >= 0; i-- {
			e := edits[i]
			var comments []byte
			for _, line := range bytes.SplitAfter(data[e.start:e.end], []byte("\n")) {
				if bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
					comments = append(comments, line...)
				}
			}
			data = splice(data, e.start, e.end, comments)
		}
	}
	if command != "" {
		block := dshRow(command)
		at := len(data)
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if strings.TrimRight(line, "\r\n") == "..." {
				data = splice(data, len(data)-at, len(data)-at, []byte(block))
				_, err = parseDSH(data)
				return data, err
			}
			at -= len(line)
		}
		data = appendBlock(data, block)
	}
	_, err = parseDSH(data)
	return data, err
}

func parseDSH(data []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil && err != io.EOF {
		return nil, err
	}
	if err := decoder.Decode(new(yaml.Node)); err != io.EOF {
		return nil, fmt.Errorf("expected one DSH YAML document")
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	if doc.Content[0].Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("DSH patches must be a YAML sequence")
	}
	return doc.Content[0], nil
}

func yamlChild(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func yamlRow(lines []string, offsets []int, index int) span {
	start := offsets[index]
	indent := len(lines[index]) - len(strings.TrimLeft(lines[index], " "))
	end := start + len(lines[index])
	// Range across all intervening lines, including comments and blank lines.
	j := index + 1
	for ; j < len(lines); j++ {
		line := lines[j]
		text := strings.TrimSpace(line)
		if text != "" && !strings.HasPrefix(text, "#") {
			if len(line)-len(strings.TrimLeft(line, " ")) <= indent {
				break
			}
			end = offsets[j] + len(line)
		}
	}
	return span{start, end}
}

func dshFlowPatch(command string) []byte {
	return []byte(`[{"insert":[{"id":"mcp-slopguard","name":"@deepseek-ai/dsh-mcp-client","config":{"serverName":"slopguard","transport":"stdio","command":` +
		strconv.Quote(command) + `,"args":["mcp"]}}]}]`)
}

func patchDSHFlow(data []byte, doc *yaml.Node, offsets []int, command string) ([]byte, error) {
	start := offsets[doc.Line-1] + doc.Column - 1
	// JSON-style YAML flow patches share the comment- and comma-aware span parser.
	p := jsonParser{data: data, pos: start, json5: true}
	_, err := p.value(0)
	if err != nil {
		return nil, fmt.Errorf("edit DSH flow patches: %w", err)
	}
	if command == "" {
		return nil, fmt.Errorf("DSH SlopGuard entry needs its original snapshot for flow config")
	}
	for _, row := range doc.Content {
		insert := yamlChild(row, "insert")
		if insert == nil {
			continue
		}
		for _, entry := range insert.Content {
			if id := yamlChild(entry, "id"); id != nil && id.Value == dshRowID {
				return nil, fmt.Errorf("existing DSH SlopGuard entry requires block YAML for editing")
			}
		}
	}
	row := dshFlowPatch(command)
	if !json.Valid(row) {
		return nil, fmt.Errorf("invalid DSH MCP patch")
	}
	data = splice(data, p.pos-1, p.pos-1, append([]byte{','}, row[1:len(row)-1]...))
	_, err = parseDSH(data)
	return data, err
}

func findDSHRow(doc *yaml.Node) bool {
	if doc == nil {
		return false
	}
	for _, row := range doc.Content {
		if insert := yamlChild(row, "insert"); insert != nil && insert.Kind == yaml.SequenceNode {
			for _, entry := range insert.Content {
				if id := yamlChild(entry, "id"); id != nil && id.Value == dshRowID {
					return true
				}
			}
		}
	}
	return false
}
