package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

type jsonNode struct {
	start, end int
	object     bool
	properties []jsonProperty
}

type jsonProperty struct {
	key          string
	start, comma int
	value        *jsonNode
}

type jsonParser struct {
	data  []byte
	pos   int
	json5 bool
}

var json5Number = regexp.MustCompile(`^[+-]?(?:Infinity|NaN|0[xX][0-9a-fA-F]+|(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)$`)

func parseJSONConfig(data []byte, json5 bool) (*jsonNode, error) {
	p := jsonParser{data: data, json5: json5}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		p.pos = 3
	}
	node, err := p.value(0)
	if err != nil {
		return nil, err
	}
	if err := p.trivia(); err != nil {
		return nil, err
	}
	if !node.object || p.pos != len(data) {
		return nil, p.invalid("expected exactly one configuration object")
	}
	return node, nil
}

func (p *jsonParser) invalid(reason string) error {
	return fmt.Errorf("invalid JSON config at byte %d: %s", p.pos, reason)
}

func (p *jsonParser) peek() byte {
	if p.pos == len(p.data) {
		return 0
	}
	return p.data[p.pos]
}

func (p *jsonParser) trivia() error {
	for p.pos < len(p.data) {
		switch p.peek() {
		case ' ', '\t', '\n', '\r':
			p.pos++
		case '/':
			switch {
			case bytes.HasPrefix(p.data[p.pos:], []byte("//")):
				for p.pos < len(p.data) && p.peek() != '\n' {
					p.pos++
				}
			case bytes.HasPrefix(p.data[p.pos:], []byte("/*")):
				n := bytes.Index(p.data[p.pos+2:], []byte("*/"))
				if n < 0 {
					return p.invalid("unterminated comment")
				}
				p.pos += n + 4
			default:
				return nil
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *jsonParser) quoted() (string, error) {
	start, quote := p.pos, p.peek()
	p.pos++
	for p.pos < len(p.data) {
		switch c := p.peek(); c {
		case '\\':
			p.pos++
			if p.pos < len(p.data) {
				p.pos++
			}
		case quote:
			p.pos++
			data := p.data[start:p.pos]
			var value string
			if quote == '"' && json.Unmarshal(data, &value) == nil {
				return value, nil
			}
			if p.json5 {
				// Decode JSON5 quotes and escape sequences without altering source bytes.
				rest := string(data[1 : len(data)-1])
				var runes []rune
				for rest != "" {
					if rest[0] == '\\' && len(rest) > 1 {
						switch rest[1] {
						case '\n':
							rest = rest[2:]
							continue
						case '\r':
							rest = rest[2:]
							if len(rest) > 0 && rest[0] == '\n' {
								rest = rest[1:]
							}
							continue
						case '0':
							runes = append(runes, 0)
							rest = rest[2:]
							continue
						}
					}
					char, _, tail, err := strconv.UnquoteChar(rest, quote)
					if err != nil {
						return "", p.invalid("invalid quoted string")
					}
					runes = append(runes, char)
					rest = tail
				}
				return string(runes), nil
			}
			return "", p.invalid("invalid string")
		case '\n', '\r':
			return "", p.invalid("unescaped newline in string")
		default:
			p.pos++
		}
	}
	return "", p.invalid("unterminated string")
}

func identifier(b byte, first bool) bool {
	return b == '_' || b == '$' || b >= 0x80 || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || !first && b >= '0' && b <= '9'
}

func (p *jsonParser) memberKey() (string, error) {
	if p.peek() == '"' || p.json5 && p.peek() == '\'' {
		return p.quoted()
	}
	if p.json5 && identifier(p.peek(), true) {
		start := p.pos
		for identifier(p.peek(), false) {
			p.pos++
		}
		return string(p.data[start:p.pos]), nil
	}
	return "", p.invalid("expected a property name")
}

func (p *jsonParser) value(depth int) (node *jsonNode, err error) {
	if depth > 256 {
		return nil, p.invalid("configuration is nested too deeply")
	}
	if err := p.trivia(); err != nil {
		return nil, err
	}
	node = &jsonNode{start: p.pos}
	switch c := p.peek(); c {
	case '{', '[':
		node.object = c == '{'
		closeByte := byte(']')
		if node.object {
			closeByte = '}'
		}
		p.pos++
		keys := map[string]bool{}
		for {
			if err := p.trivia(); err != nil {
				return nil, err
			}
			if p.peek() == closeByte {
				p.pos++
				break
			}
			var prop jsonProperty
			if node.object {
				prop.start, prop.comma = p.pos, -1
				prop.key, err = p.memberKey()
				if err != nil {
					return nil, err
				}
				if keys[prop.key] {
					return nil, p.invalid("duplicate property " + prop.key)
				}
				keys[prop.key] = true
				if err := p.trivia(); err != nil {
					return nil, err
				}
				if p.peek() != ':' {
					return nil, p.invalid("expected a colon")
				}
				p.pos++
			}
			prop.value, err = p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			if err := p.trivia(); err != nil {
				return nil, err
			}
			if p.peek() == ',' {
				prop.comma = p.pos
				p.pos++
			} else if p.peek() != closeByte {
				return nil, p.invalid("expected a comma or closing delimiter")
			}
			if node.object {
				node.properties = append(node.properties, prop)
			}
		}
	case '"', '\'':
		if c == '\'' && !p.json5 {
			return nil, p.invalid("expected a double-quoted string")
		}
		if _, err := p.quoted(); err != nil {
			return nil, err
		}
	default:
		for p.pos < len(p.data) &&
			!bytes.ContainsRune([]byte(",]}:/ \n\r\t"), rune(p.peek())) {
			p.pos++
		}
		token := p.data[node.start:p.pos]
		if len(token) == 0 || !json.Valid(token) && !(p.json5 && json5Number.Match(token)) {
			return nil, p.invalid("invalid value")
		}
	}
	node.end = p.pos
	return node, nil
}

func jsonPropertyIndex(node *jsonNode, key string) int {
	for i, prop := range node.properties {
		if prop.key == key {
			return i
		}
	}
	return -1
}

func jsonNodeAt(root *jsonNode, path []string) *jsonNode {
	for _, key := range path {
		i := jsonPropertyIndex(root, key)
		if i < 0 {
			return nil
		}
		root = root.properties[i].value
	}
	return root
}

func patchJSON(data []byte, json5 bool, path []string, replacement []byte) ([]byte, error) {
	if len(data) == 0 {
		data = []byte("{}\n")
	}
	root, err := parseJSONConfig(data, json5)
	if err != nil {
		return nil, err
	}
	parent := root
	for i, key := range path {
		if !parent.object {
			return nil, fmt.Errorf("MCP parent for %q must be an object", key)
		}
		pos := jsonPropertyIndex(parent, key)
		if pos < 0 {
			if replacement == nil {
				return data, nil
			}
			value := replacement
			for j := len(path) - 1; j > i; j-- {
				value = []byte("{" + strconv.Quote(path[j]) + ":" + string(value) + "}")
			}
			text := strconv.Quote(key) + ":" + string(value)
			at := parent.end - 1
			if n := len(parent.properties); n > 0 {
				at = parent.properties[n-1].value.end
				text = "," + text
			}
			data = splice(data, at, at, []byte(text))
			break
		}
		prop := parent.properties[pos]
		if i != len(path)-1 {
			parent = prop.value
			continue
		}
		if replacement != nil {
			data = splice(data, prop.value.start, prop.value.end, replacement)
		} else {
			comma := prop.comma
			if comma < 0 && pos > 0 {
				comma = parent.properties[pos-1].comma
			}
			data = splice(data, prop.start, prop.value.end, nil)
			if comma >= prop.value.end {
				comma -= prop.value.end - prop.start
			}
			if comma >= 0 {
				data = splice(data, comma, comma+1, nil)
			}
		}
	}
	_, err = parseJSONConfig(data, json5)
	return data, err
}

func splice(data []byte, start, end int, replacement []byte) []byte {
	result := make([]byte, 0, len(data)-(end-start)+len(replacement))
	result = append(result, data[:start]...)
	result = append(result, replacement...)
	return append(result, data[end:]...)
}
