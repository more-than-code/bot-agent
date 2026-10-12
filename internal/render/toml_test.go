package render_test

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// parseTOML reads the snippet subset this renderer emits: comments, bare keys,
// basic and literal strings, integers, booleans, arrays, and dotted tables.
func parseTOML(text string) (map[string]any, error) {
	p := &tomlParser{s: text, root: map[string]any{}}
	p.cur = p.root
	for {
		p.skipGaps()
		if p.eof() {
			return p.root, nil
		}
		if p.peek() == '[' {
			if err := p.parseTable(); err != nil {
				return nil, err
			}
			continue
		}
		if err := p.parseKeyValue(); err != nil {
			return nil, err
		}
	}
}

type tomlParser struct {
	s    string
	i    int
	root map[string]any
	cur  map[string]any
}

func (p *tomlParser) eof() bool { return p.i >= len(p.s) }

func (p *tomlParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *tomlParser) skipGaps() {
	for !p.eof() {
		c := p.peek()
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.i++
			continue
		}
		if c == '#' {
			p.skipLine()
			continue
		}
		return
	}
}

func (p *tomlParser) skipLine() {
	for !p.eof() && p.peek() != '\n' {
		p.i++
	}
}

func (p *tomlParser) errAt(message string) error {
	start := p.i - 20
	if start < 0 {
		start = 0
	}
	end := p.i + 20
	if end > len(p.s) {
		end = len(p.s)
	}
	return fmt.Errorf("toml at %d: %s near %q", p.i, message, p.s[start:end])
}

func (p *tomlParser) parseTable() error {
	p.i++ // '['
	array := false
	if !p.eof() && p.peek() == '[' {
		array = true
		p.i++
	}
	closeMark := "]"
	if array {
		closeMark = "]]"
	}
	rel := strings.Index(p.s[p.i:], closeMark)
	if rel < 0 {
		return p.errAt("unclosed table")
	}
	name := strings.TrimSpace(p.s[p.i : p.i+rel])
	p.i += rel + len(closeMark)
	if err := p.endStatement(); err != nil {
		return err
	}
	parts := strings.Split(name, ".")
	if name == "" || strings.ContainsAny(name, " \t") {
		return fmt.Errorf("toml: bad table %q", name)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("toml: bad table %q", name)
		}
	}
	if array {
		return p.defineArray(parts)
	}
	return p.defineTable(parts)
}

func (p *tomlParser) container(parts []string) (map[string]any, error) {
	cur := p.root
	for _, part := range parts {
		v, ok := cur[part]
		if !ok {
			next := map[string]any{}
			cur[part] = next
			cur = next
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			cur = t
		case []any:
			if len(t) == 0 {
				return nil, fmt.Errorf("toml: empty array %s", part)
			}
			last, ok := t[len(t)-1].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("toml: %s element is not a table", part)
			}
			cur = last
		default:
			return nil, fmt.Errorf("toml: %s is not a table", part)
		}
	}
	return cur, nil
}

func (p *tomlParser) defineArray(parts []string) error {
	parent, err := p.container(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	key := parts[len(parts)-1]
	var arr []any
	if existing, ok := parent[key]; ok {
		var isArray bool
		arr, isArray = existing.([]any)
		if !isArray {
			return fmt.Errorf("toml: %s is not an array of tables", key)
		}
	}
	elem := map[string]any{}
	parent[key] = append(arr, elem)
	p.cur = elem
	return nil
}

func (p *tomlParser) defineTable(parts []string) error {
	parent, err := p.container(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	key := parts[len(parts)-1]
	if _, ok := parent[key]; ok {
		return fmt.Errorf("toml: duplicate table %s", key)
	}
	elem := map[string]any{}
	parent[key] = elem
	p.cur = elem
	return nil
}

func (p *tomlParser) parseKeyValue() error {
	key, err := p.parseKey()
	if err != nil {
		return err
	}
	p.skipSpaces()
	if p.eof() || p.peek() != '=' {
		return p.errAt("expected =")
	}
	p.i++
	p.skipSpaces()
	value, err := p.parseValue()
	if err != nil {
		return err
	}
	if err := p.endStatement(); err != nil {
		return err
	}
	if _, ok := p.cur[key]; ok {
		return fmt.Errorf("toml: duplicate key %s", key)
	}
	p.cur[key] = value
	return nil
}

func (p *tomlParser) parseKey() (string, error) {
	start := p.i
	for !p.eof() {
		c := p.peek()
		if c == '_' || c == '-' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			p.i++
			continue
		}
		break
	}
	if p.i == start {
		return "", p.errAt("expected key")
	}
	return p.s[start:p.i], nil
}

func (p *tomlParser) skipSpaces() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.i++
	}
}

func (p *tomlParser) endStatement() error {
	p.skipSpaces()
	if p.eof() {
		return nil
	}
	if p.peek() == '#' {
		p.skipLine()
		return nil
	}
	if p.peek() == '\n' || p.peek() == '\r' {
		return nil
	}
	return p.errAt("trailing junk")
}

func (p *tomlParser) parseValue() (any, error) {
	if p.eof() {
		return nil, p.errAt("expected value")
	}
	switch p.peek() {
	case '"':
		return p.parseBasicString()
	case '\'':
		return p.parseLiteralString()
	case '[':
		return p.parseArray()
	case 't':
		if strings.HasPrefix(p.s[p.i:], "true") && p.wordEnd(4) {
			p.i += 4
			return true, nil
		}
	case 'f':
		if strings.HasPrefix(p.s[p.i:], "false") && p.wordEnd(5) {
			p.i += 5
			return false, nil
		}
	}
	return p.parseInt()
}

func (p *tomlParser) wordEnd(n int) bool {
	j := p.i + n
	if j > len(p.s) {
		return false
	}
	if j == len(p.s) {
		return true
	}
	c := rune(p.s[j])
	return !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_'
}

func (p *tomlParser) parseBasicString() (string, error) {
	p.i++ // '"'
	var b strings.Builder
	for !p.eof() {
		c := p.peek()
		if c == '"' {
			p.i++
			return b.String(), nil
		}
		if c == '\n' {
			return "", p.errAt("newline in string")
		}
		if c != '\\' {
			b.WriteByte(c)
			p.i++
			continue
		}
		p.i++
		if p.eof() {
			return "", p.errAt("bad escape")
		}
		esc := p.peek()
		p.i++
		switch esc {
		case '"', '\\', '/':
			b.WriteByte(esc)
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'u', 'U':
			width := 4
			if esc == 'U' {
				width = 8
			}
			if p.i+width > len(p.s) {
				return "", p.errAt("short unicode escape")
			}
			n, err := strconv.ParseUint(p.s[p.i:p.i+width], 16, 32)
			if err != nil {
				return "", p.errAt("bad unicode escape")
			}
			p.i += width
			b.WriteRune(rune(n))
		default:
			return "", p.errAt("bad escape")
		}
	}
	return "", p.errAt("unclosed string")
}

func (p *tomlParser) parseLiteralString() (string, error) {
	p.i++ // '\''
	start := p.i
	for !p.eof() && p.peek() != '\'' {
		if p.peek() == '\n' {
			return "", p.errAt("newline in literal")
		}
		p.i++
	}
	if p.eof() {
		return "", p.errAt("unclosed literal")
	}
	text := p.s[start:p.i]
	p.i++
	return text, nil
}

func (p *tomlParser) parseArray() (any, error) {
	p.i++ // '['
	var items []any
	for {
		p.skipGaps()
		if p.eof() {
			return nil, p.errAt("unclosed array")
		}
		if p.peek() == ']' {
			p.i++
			return items, nil
		}
		item, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		p.skipGaps()
		if p.eof() {
			return nil, p.errAt("unclosed array")
		}
		if p.peek() == ',' {
			p.i++
			continue
		}
		if p.peek() == ']' {
			p.i++
			return items, nil
		}
		return nil, p.errAt("expected comma or ]")
	}
}

func (p *tomlParser) parseInt() (any, error) {
	start := p.i
	if p.peek() == '+' || p.peek() == '-' {
		p.i++
	}
	digits := p.i
	for !p.eof() && p.peek() >= '0' && p.peek() <= '9' {
		p.i++
	}
	if p.i == digits {
		return nil, p.errAt("expected value")
	}
	if !p.eof() && (p.peek() == '.' || p.peek() == 'e' || p.peek() == 'E') {
		return nil, p.errAt("floats are not used")
	}
	n, err := strconv.Atoi(p.s[start:p.i])
	if err != nil {
		return nil, p.errAt("bad integer")
	}
	return n, nil
}
