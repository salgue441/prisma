package graphqlguard

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"

	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
)

type fieldRule struct {
	coordinate string
	scopes     []string
	strip      bool
}

func (f *Filter) loadSchema() error {
	sdl := f.cfg.Schema
	if f.cfg.SchemaFile != "" {
		b, err := os.ReadFile(f.cfg.SchemaFile)
		if err != nil {
			return fmt.Errorf("graphql schema: %w", err)
		}
		sdl = string(b)
	}
	if sdl == "" {
		return nil
	}
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: "schema", Input: sdl})
	if err != nil {
		return fmt.Errorf("graphql schema: %w", err)
	}
	f.schema = schema
	f.rules = map[string]fieldRule{}
	for _, r := range f.cfg.FieldAuthz {
		typ, field, _ := strings.Cut(r.Coordinate, ".")
		def := schema.Types[typ]
		if def == nil {
			return fmt.Errorf("graphql fieldAuthz: type %q is not in the schema", typ)
		}
		if field != "*" && def.Fields.ForName(field) == nil {
			return fmt.Errorf("graphql fieldAuthz: %s has no field %q", typ, field)
		}
		f.rules[r.Coordinate] = fieldRule{coordinate: r.Coordinate, scopes: r.Scopes, strip: r.OnDeny == "strip"}
	}
	return nil
}

// authorizeFields applies field rules to every field reachable from op
// (including through fragments). Denied "strip" fields are removed from the
// document; a denied "reject" field fails the whole operation.
func (f *Filter) authorizeFields(doc *ast.QueryDocument, op *ast.OperationDefinition, p *pipeline.Principal) (stripped bool, deny *gwerr.Error) {
	visited := map[string]bool{}
	var walk func(ss ast.SelectionSet) ast.SelectionSet
	walk = func(ss ast.SelectionSet) ast.SelectionSet {
		out := make(ast.SelectionSet, 0, len(ss))
		for _, sel := range ss {
			switch s := sel.(type) {
			case *ast.Field:
				if rule, ok := f.ruleFor(s); ok && !hasScopes(p, rule.scopes) {
					if !rule.strip {
						deny = gwerr.Forbidden(fmt.Sprintf("access to field %s denied", rule.coordinate))
						return ss
					}
					stripped = true
					continue
				}
				if len(s.SelectionSet) > 0 {
					s.SelectionSet = nonEmpty(walk(s.SelectionSet))
				}
			case *ast.InlineFragment:
				s.SelectionSet = nonEmpty(walk(s.SelectionSet))
			case *ast.FragmentSpread:
				if !visited[s.Name] {
					visited[s.Name] = true
					if fd := doc.Fragments.ForName(s.Name); fd != nil {
						fd.SelectionSet = nonEmpty(walk(fd.SelectionSet))
					}
				}
			}
			if deny != nil {
				return ss
			}
			out = append(out, sel)
		}
		return out
	}
	op.SelectionSet = nonEmpty(walk(op.SelectionSet))
	return stripped, deny
}

func (f *Filter) ruleFor(field *ast.Field) (fieldRule, bool) {
	if field.ObjectDefinition == nil {
		return fieldRule{}, false
	}
	typ := field.ObjectDefinition.Name
	if r, ok := f.rules[typ+"."+field.Name]; ok {
		return r, true
	}
	r, ok := f.rules[typ+".*"]
	return r, ok && !strings.HasPrefix(field.Name, "__")
}

// nonEmpty keeps a selection set valid after stripping every field.
func nonEmpty(ss ast.SelectionSet) ast.SelectionSet {
	if len(ss) == 0 {
		return ast.SelectionSet{&ast.Field{Alias: "__typename", Name: "__typename"}}
	}
	return ss
}

func hasScopes(p *pipeline.Principal, scopes []string) bool {
	for _, s := range scopes {
		if !p.HasScope(s) {
			return false
		}
	}
	return true
}

func format(doc *ast.QueryDocument) string {
	var b bytes.Buffer
	formatter.NewFormatter(&b).FormatQueryDocument(doc)
	return b.String()
}
