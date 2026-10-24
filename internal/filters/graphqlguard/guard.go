// Package graphqlguard statically analyzes GraphQL operations before they
// reach the upstream: depth, aliases, root fields, cost, batching and
// introspection. It needs no schema.
package graphqlguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
)

// listArgs multiply the cost of a field's children (pagination arguments).
var listArgs = []string{"first", "last", "limit", "take", "pageSize"}

const maxCost = 1 << 30

type Filter struct {
	cfg     *config.GraphQL
	maxBody int64
}

func New(cfg *config.GraphQL, maxBody int64) *Filter {
	return &Filter{cfg: cfg, maxBody: maxBody}
}

func (f *Filter) Name() string { return "graphql_guard" }

type request struct {
	Query         string          `json:"query"`
	OperationName string          `json:"operationName"`
	Variables     map[string]any  `json:"variables"`
	Extensions    json.RawMessage `json:"extensions"`
}

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	reqs, err := f.read(c)
	if err != nil {
		var ge *gwerr.Error
		if errors.As(err, &ge) {
			return ge
		}
		return gwerr.BadRequest("BAD_REQUEST", err.Error())
	}
	if f.cfg.MaxBatch > 0 && len(reqs) > f.cfg.MaxBatch {
		return gwerr.TooComplex(fmt.Sprintf("batch of %d operations exceeds limit %d", len(reqs), f.cfg.MaxBatch))
	}
	total := &pipeline.GraphQLInfo{}
	for i, r := range reqs {
		// Persisted-query requests carry only a hash; the full document was
		// analyzed when it was registered.
		if r.Query == "" {
			total.Cost++
			continue
		}
		info, err := Analyze(r.Query, r.OperationName, r.Variables)
		if err != nil {
			return gwerr.BadRequest("GRAPHQL_PARSE_FAILED", err.Error())
		}
		if err := f.check(info); err != nil {
			return err
		}
		if i == 0 {
			total.OperationType, total.OperationName = info.OperationType, info.OperationName
		}
		total.Depth = max(total.Depth, info.Depth)
		total.Aliases += info.Aliases
		total.Cost = min(total.Cost+info.Cost, maxCost)
		if info.Introspection && f.cfg.BlockIntrospection {
			return gwerr.Forbidden("introspection is disabled")
		}
	}
	if f.cfg.MaxCost > 0 && total.Cost > f.cfg.MaxCost {
		return gwerr.TooComplex(fmt.Sprintf("query cost %d exceeds limit %d", total.Cost, f.cfg.MaxCost))
	}
	c.GraphQL = total
	return nil
}

func (f *Filter) check(info *Info) *gwerr.Error {
	switch {
	case f.cfg.MaxDepth > 0 && info.Depth > f.cfg.MaxDepth:
		return gwerr.TooComplex(fmt.Sprintf("query depth %d exceeds limit %d", info.Depth, f.cfg.MaxDepth))
	case f.cfg.MaxAliases > 0 && info.Aliases > f.cfg.MaxAliases:
		return gwerr.TooComplex(fmt.Sprintf("query uses %d aliases, limit is %d", info.Aliases, f.cfg.MaxAliases))
	case f.cfg.MaxRootFields > 0 && info.RootFields > f.cfg.MaxRootFields:
		return gwerr.TooComplex(fmt.Sprintf("query selects %d root fields, limit is %d", info.RootFields, f.cfg.MaxRootFields))
	}
	return nil
}

// read extracts operations from GET query params, application/graphql bodies,
// or JSON bodies (single or batched). The body is buffered and restored so it
// can still be proxied and replayed on retries.
func (f *Filter) read(c *pipeline.Context) ([]request, error) {
	r := c.Request
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		var vars map[string]any
		if v := q.Get("variables"); v != "" {
			if err := json.Unmarshal([]byte(v), &vars); err != nil {
				return nil, fmt.Errorf("invalid variables: %w", err)
			}
		}
		return []request{{Query: q.Get("query"), OperationName: q.Get("operationName"), Variables: vars}}, nil
	}
	if r.Method != http.MethodPost {
		return nil, gwerr.BadRequest("BAD_REQUEST", "graphql requires GET or POST")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, f.maxBody+1))
	r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > f.maxBody {
		return nil, gwerr.PayloadTooLarge()
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))

	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "application/graphql" {
		return []request{{Query: string(body)}}, nil
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []request
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("invalid graphql batch: %w", err)
		}
		if len(batch) == 0 {
			return nil, errors.New("empty graphql batch")
		}
		return batch, nil
	}
	var one request
	if err := json.Unmarshal(body, &one); err != nil {
		return nil, fmt.Errorf("invalid graphql request: %w", err)
	}
	if one.Query == "" && len(one.Extensions) == 0 {
		return nil, errors.New("missing query")
	}
	return []request{one}, nil
}

// Info is the static analysis result for one operation.
type Info struct {
	OperationType string
	OperationName string
	Depth         int
	Aliases       int
	RootFields    int
	Cost          int
	Introspection bool
}

// Analyze parses query and measures the selected operation. Cost counts every
// field once, multiplied by the pagination size of its enclosing lists.
func Analyze(query, operationName string, vars map[string]any) (*Info, error) {
	doc, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return nil, err
	}
	var op *ast.OperationDefinition
	switch {
	case operationName != "":
		for _, o := range doc.Operations {
			if o.Name == operationName {
				op = o
			}
		}
		if op == nil {
			return nil, fmt.Errorf("operation %q not found", operationName)
		}
	case len(doc.Operations) == 1:
		op = doc.Operations[0]
	case len(doc.Operations) == 0:
		return nil, errors.New("document contains no operations")
	default:
		return nil, errors.New("operationName is required for documents with multiple operations")
	}
	a := &analyzer{doc: doc, vars: vars, info: &Info{OperationType: string(op.Operation), OperationName: op.Name}, active: map[string]bool{}}
	a.info.RootFields = a.countRoot(op.SelectionSet)
	a.info.Depth = a.walk(op.SelectionSet, 1, 1)
	return a.info, nil
}

type analyzer struct {
	doc    *ast.QueryDocument
	vars   map[string]any
	info   *Info
	active map[string]bool // fragments on the current path (cycle guard)
}

func (a *analyzer) countRoot(ss ast.SelectionSet) int {
	n := 0
	for _, sel := range ss {
		switch s := sel.(type) {
		case *ast.Field:
			n++
		case *ast.InlineFragment:
			n += a.countRoot(s.SelectionSet)
		case *ast.FragmentSpread:
			if fd := a.fragment(s.Name); fd != nil && !a.active[s.Name] {
				a.active[s.Name] = true
				n += a.countRoot(fd.SelectionSet)
				delete(a.active, s.Name)
			}
		}
	}
	return n
}

// walk returns the max depth below ss and accumulates cost and aliases.
func (a *analyzer) walk(ss ast.SelectionSet, depth, mult int) int {
	maxDepth := 0
	for _, sel := range ss {
		switch s := sel.(type) {
		case *ast.Field:
			if s.Name == "__schema" || s.Name == "__type" {
				a.info.Introspection = true
			}
			if s.Alias != "" && s.Alias != s.Name {
				a.info.Aliases++
			}
			a.info.Cost = min(a.info.Cost+mult, maxCost)
			d := depth
			if len(s.SelectionSet) > 0 {
				d = max(depth, a.walk(s.SelectionSet, depth+1, min(mult*a.listSize(s), maxCost)))
			}
			maxDepth = max(maxDepth, d)
		case *ast.InlineFragment:
			maxDepth = max(maxDepth, a.walk(s.SelectionSet, depth, mult))
		case *ast.FragmentSpread:
			fd := a.fragment(s.Name)
			if fd == nil || a.active[s.Name] {
				continue
			}
			a.active[s.Name] = true
			maxDepth = max(maxDepth, a.walk(fd.SelectionSet, depth, mult))
			delete(a.active, s.Name)
		}
	}
	return maxDepth
}

func (a *analyzer) fragment(name string) *ast.FragmentDefinition {
	for _, f := range a.doc.Fragments {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func (a *analyzer) listSize(f *ast.Field) int {
	for _, arg := range f.Arguments {
		for _, name := range listArgs {
			if arg.Name != name || arg.Value == nil {
				continue
			}
			raw := arg.Value.Raw
			if arg.Value.Kind == ast.Variable {
				raw = fmt.Sprint(a.vars[raw])
			}
			if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}
