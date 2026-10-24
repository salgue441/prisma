package graphqlguard

import "testing"

func TestAnalyze(t *testing.T) {
	cases := []struct {
		name                        string
		query, op                   string
		vars                        map[string]any
		depth, aliases, roots, cost int
		introspection               bool
	}{
		{name: "flat", query: `{ a b c }`, depth: 1, roots: 3, cost: 3},
		{name: "nested", query: `{ a { b { c } } }`, depth: 3, roots: 1, cost: 3},
		{name: "aliases", query: `{ x: a y: a z: a }`, depth: 1, aliases: 3, roots: 3, cost: 3},
		{name: "list multiplier", query: `{ users(first: 10) { name email } }`, depth: 2, roots: 1, cost: 1 + 20},
		{name: "variable multiplier", query: `query($n: Int) { users(first: $n) { name } }`, vars: map[string]any{"n": 50.0}, depth: 2, roots: 1, cost: 51},
		{name: "fragments", query: `query Q { me { ...F } } fragment F on User { name friends { name } }`, depth: 3, roots: 1, cost: 4},
		{name: "recursive fragment terminates", query: `{ me { ...F } } fragment F on User { friends { ...F } }`, depth: 2, roots: 1, cost: 2},
		{name: "introspection", query: `{ __schema { types { name } } }`, depth: 3, roots: 1, cost: 3, introspection: true},
		{name: "typename is fine", query: `{ me { __typename } }`, depth: 2, roots: 1, cost: 2},
		{name: "named op", query: `query A { a } query B { b { c } }`, op: "B", depth: 2, roots: 1, cost: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := Analyze(tc.query, tc.op, tc.vars)
			if err != nil {
				t.Fatal(err)
			}
			if info.Depth != tc.depth || info.Aliases != tc.aliases || info.RootFields != tc.roots ||
				info.Cost != tc.cost || info.Introspection != tc.introspection {
				t.Errorf("got %+v", info)
			}
		})
	}
}

func TestAnalyzeErrors(t *testing.T) {
	for _, q := range []struct{ query, op string }{
		{`{ a `, ""},
		{`query A { a } query B { b }`, ""},
		{`query A { a }`, "Missing"},
		{`fragment F on T { a }`, ""},
	} {
		if _, err := Analyze(q.query, q.op, nil); err == nil {
			t.Errorf("Analyze(%q, %q) should fail", q.query, q.op)
		}
	}
}
