package masking

import (
	"fmt"
	"strconv"
	"strings"
)

type segKind uint8

const (
	segChild segKind = iota
	segRecursive
	segWildcard
	segIndex
)

type seg struct {
	kind segKind
	name string
	idx  int
}

// parseSelector supports a JSONPath subset: $.a.b, $..a, $.a[*].b, $.a.*, $.a[0].
func parseSelector(s string) ([]seg, error) {
	rest, ok := strings.CutPrefix(s, "$")
	if !ok {
		return nil, fmt.Errorf("selector %q must start with $", s)
	}
	var segs []seg
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, ".."):
			name, r := readName(rest[2:])
			if name == "" {
				return nil, fmt.Errorf("selector %q: empty name after ..", s)
			}
			segs, rest = append(segs, seg{kind: segRecursive, name: name}), r
		case strings.HasPrefix(rest, ".*"):
			segs, rest = append(segs, seg{kind: segWildcard}), rest[2:]
		case strings.HasPrefix(rest, "."):
			name, r := readName(rest[1:])
			if name == "" {
				return nil, fmt.Errorf("selector %q: empty name", s)
			}
			segs, rest = append(segs, seg{kind: segChild, name: name}), r
		case strings.HasPrefix(rest, "[*]"):
			segs, rest = append(segs, seg{kind: segWildcard}), rest[3:]
		case strings.HasPrefix(rest, "["):
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("selector %q: unclosed [", s)
			}
			inner := rest[1:end]
			if q := strings.Trim(inner, `'"`); q != inner {
				segs = append(segs, seg{kind: segChild, name: q})
			} else {
				i, err := strconv.Atoi(inner)
				if err != nil || i < 0 {
					return nil, fmt.Errorf("selector %q: invalid index %q", s, inner)
				}
				segs = append(segs, seg{kind: segIndex, idx: i})
			}
			rest = rest[end+1:]
		default:
			return nil, fmt.Errorf("selector %q: unexpected %q", s, rest)
		}
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("selector %q selects the whole document", s)
	}
	return segs, nil
}

func readName(s string) (string, string) {
	i := strings.IndexAny(s, ".[")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// action transforms a matched value; keep=false removes it from its parent.
type action func(v any) (nv any, keep bool)

func walk(v any, segs []seg, act action) any {
	s, rest := segs[0], segs[1:]
	switch s.kind {
	case segChild:
		if m, ok := v.(map[string]any); ok {
			if child, ok := m[s.name]; ok {
				applyAt(m, s.name, child, rest, act)
			}
		}
	case segIndex:
		if a, ok := v.([]any); ok && s.idx < len(a) {
			a[s.idx] = applyElem(a[s.idx], rest, act)
		}
	case segWildcard:
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				applyAt(t, k, child, rest, act)
			}
		case []any:
			for i := range t {
				t[i] = applyElem(t[i], rest, act)
			}
		}
	case segRecursive:
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if k == s.name {
					applyAt(t, k, child, rest, act)
					if len(rest) == 0 {
						continue // don't descend into a value we just masked
					}
					if nv, ok := t[k]; ok {
						t[k] = walk(nv, segs, act)
					}
					continue
				}
				t[k] = walk(child, segs, act)
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i], segs, act)
			}
		}
	}
	return v
}

func applyAt(m map[string]any, k string, child any, rest []seg, act action) {
	if len(rest) == 0 {
		if nv, keep := act(child); keep {
			m[k] = nv
		} else {
			delete(m, k)
		}
		return
	}
	m[k] = walk(child, rest, act)
}

func applyElem(child any, rest []seg, act action) any {
	if len(rest) == 0 {
		if nv, keep := act(child); keep {
			return nv
		}
		return nil
	}
	return walk(child, rest, act)
}
