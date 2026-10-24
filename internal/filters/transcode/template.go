package transcode

import (
	"fmt"
	"net/url"
	"strings"
)

// template is a compiled google.api.http path template:
//
//	Template = "/" Segments [ Verb ] ;
//	Segments = Segment { "/" Segment } ;
//	Segment  = "*" | "**" | LITERAL | Variable ;
//	Variable = "{" FieldPath [ "=" Segments ] "}" ;
//	Verb     = ":" LITERAL ;
type template struct {
	segs []segment
	vars []variable
	verb string
}

type segKind int

const (
	segLiteral segKind = iota
	segStar            // one segment
	segDeep            // zero or more segments; only last
)

type segment struct {
	kind segKind
	lit  string
}

// variable captures segs[start:end] into a request field.
type variable struct {
	field      []string
	start, end int
}

func parseTemplate(s string) (*template, error) {
	if !strings.HasPrefix(s, "/") {
		return nil, fmt.Errorf("template %q must start with /", s)
	}
	t := &template{}
	body := s[1:]
	// The verb follows the last ':' outside braces.
	depth := 0
	for i := len(body) - 1; i >= 0; i-- {
		switch body[i] {
		case '}':
			depth++
		case '{':
			depth--
		case ':':
			if depth == 0 && !strings.Contains(body[i:], "/") {
				t.verb, body = body[i+1:], body[:i]
				i = -1
			}
		}
	}
	if err := t.parseSegments(body, nil); err != nil {
		return nil, fmt.Errorf("template %q: %w", s, err)
	}
	for i, sg := range t.segs {
		if sg.kind == segDeep && i != len(t.segs)-1 {
			return nil, fmt.Errorf("template %q: ** must be the last segment", s)
		}
	}
	return t, nil
}

func (t *template) parseSegments(s string, field []string) error {
	for len(s) > 0 {
		var part string
		if s[0] == '{' {
			end := strings.IndexByte(s, '}')
			if end < 0 {
				return fmt.Errorf("unclosed variable")
			}
			part, s = s[:end+1], s[end+1:]
		} else {
			end := strings.IndexByte(s, '/')
			if end < 0 {
				end = len(s)
			}
			part, s = s[:end], s[end:]
		}
		s = strings.TrimPrefix(s, "/")
		switch {
		case part == "":
			return fmt.Errorf("empty segment")
		case part == "*":
			t.segs = append(t.segs, segment{kind: segStar})
		case part == "**":
			t.segs = append(t.segs, segment{kind: segDeep})
		case part[0] == '{':
			if field != nil {
				return fmt.Errorf("nested variables are not allowed")
			}
			inner := part[1 : len(part)-1]
			name, sub, hasSub := strings.Cut(inner, "=")
			if name == "" {
				return fmt.Errorf("variable without a field name")
			}
			fp := strings.Split(name, ".")
			start := len(t.segs)
			if !hasSub {
				sub = "*"
			}
			if err := t.parseSegments(sub, fp); err != nil {
				return err
			}
			t.vars = append(t.vars, variable{field: fp, start: start, end: len(t.segs)})
		default:
			t.segs = append(t.segs, segment{kind: segLiteral, lit: part})
		}
	}
	return nil
}

// match matches an escaped request path and returns variable bindings
// (field path joined by "." -> value).
func (t *template) match(escapedPath string) (map[string]string, bool) {
	p := strings.TrimPrefix(escapedPath, "/")
	if t.verb != "" {
		cut, ok := strings.CutSuffix(p, ":"+t.verb)
		if !ok {
			return nil, false
		}
		p = cut
	}
	var parts []string
	if p != "" {
		parts = strings.Split(p, "/")
	}
	// spans[i] is the range of request segments matched by template segs[i].
	spans := make([][2]int, len(t.segs))
	pos := 0
	for i, sg := range t.segs {
		switch sg.kind {
		case segLiteral:
			if pos >= len(parts) || parts[pos] != sg.lit {
				return nil, false
			}
			spans[i] = [2]int{pos, pos + 1}
			pos++
		case segStar:
			if pos >= len(parts) || parts[pos] == "" {
				return nil, false
			}
			spans[i] = [2]int{pos, pos + 1}
			pos++
		case segDeep:
			spans[i] = [2]int{pos, len(parts)}
			pos = len(parts)
		}
	}
	if pos != len(parts) {
		return nil, false
	}
	vals := map[string]string{}
	for _, v := range t.vars {
		if v.start == v.end {
			continue
		}
		from, to := spans[v.start][0], spans[v.end-1][1]
		if from == to {
			continue // ** matched nothing
		}
		segs := make([]string, 0, to-from)
		for _, s := range parts[from:to] {
			u, err := url.PathUnescape(s)
			if err != nil {
				return nil, false
			}
			segs = append(segs, u)
		}
		vals[strings.Join(v.field, ".")] = strings.Join(segs, "/")
	}
	return vals, true
}

// literals counts literal segments; more literals means a more specific
// template, which wins when several rules match.
func (t *template) literals() int {
	n := 0
	for _, s := range t.segs {
		if s.kind == segLiteral {
			n++
		}
	}
	return n
}
