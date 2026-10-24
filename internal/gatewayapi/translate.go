package gatewayapi

import (
	"cmp"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"prisma/internal/config"
	"prisma/internal/k8s"
)

// Input is the observed cluster state, keyed like k8s.Object.Key().
type Input struct {
	ControllerName string
	// Base is the static configuration (plugins, redis, consumers) used to
	// validate PrismaPolicies.
	Base *config.Config
	// Discovery is copied into generated clusters (API server access).
	Discovery config.KubernetesDiscovery

	GatewayClasses  map[string]k8s.Object
	Gateways        map[string]k8s.Object
	HTTPRoutes      map[string]k8s.Object
	GRPCRoutes      map[string]k8s.Object
	ReferenceGrants map[string]k8s.Object
	Services        map[string]k8s.Object
	Secrets         map[string]k8s.Object
	Policies        map[string]k8s.Object
}

type Output struct {
	Listeners []config.Listener
	Clusters  []config.Cluster
	Routes    []config.Route
	Policies  map[string]config.Policy
	Status    Status
}

// Translate is pure: the same input always yields the same output, in a
// stable order, so the generated configuration hash only changes when the
// cluster state does.
func Translate(in Input) Output {
	t := &translator{in: in, out: Output{Policies: map[string]config.Policy{}},
		clusters: map[string]config.Cluster{}, ports: map[int]*portListener{}, gwListeners: map[string][]*attachable{}}
	t.translateClasses()
	t.translateGateways()
	t.translatePolicies()
	t.translateRoutes()
	t.finish()
	return t.out
}

type translator struct {
	in  Input
	out Output

	ours        map[string]bool // gateway class names
	gateways    map[string]*gatewayState
	ports       map[int]*portListener
	gwListeners map[string][]*attachable // gateway key -> listeners
	clusters    map[string]config.Cluster

	policyByTarget map[string][]string // "Kind/ns/name" -> policy config names
	policyTargets  map[string][]policyTarget
}

type gatewayState struct {
	gw     gateway
	status *GatewayStatus
}

// portListener is one gateway listener socket; several Gateway listeners
// (possibly from several Gateways) on the same port share it.
type portListener struct {
	name     string
	port     int
	protocol string
	certs    []config.Certificate
}

type attachable struct {
	gwKey    string
	gwNS     string
	l        listener
	merged   *portListener
	status   *ListenerStatus
	accepted bool
}

func gatewayRefKey(ns, name string) string { return ns + "/" + name }

// --- GatewayClasses and Gateways ---

func (t *translator) translateClasses() {
	t.ours = map[string]bool{}
	for _, o := range sortedObjects(t.in.GatewayClasses) {
		var gc gatewayClass
		if json.Unmarshal(o.Raw, &gc) != nil || gc.Spec.ControllerName != t.in.ControllerName {
			continue
		}
		t.ours[gc.Metadata.Name] = true
		t.out.Status.Classes = append(t.out.Status.Classes, ClassStatus{
			Name: gc.Metadata.Name, Generation: gc.Metadata.Generation,
			Conditions: []Condition{cond("Accepted", true, "Accepted", "handled by "+t.in.ControllerName)},
		})
	}
}

func (t *translator) translateGateways() {
	t.gateways = map[string]*gatewayState{}
	var gws []gateway
	for _, o := range t.in.Gateways {
		var gw gateway
		if json.Unmarshal(o.Raw, &gw) == nil && t.ours[gw.Spec.GatewayClassName] {
			gws = append(gws, gw)
		}
	}
	slices.SortFunc(gws, func(a, b gateway) int { return olderFirst(a.Metadata, b.Metadata) })

	for _, gw := range gws {
		key := gatewayRefKey(gw.Metadata.Namespace, gw.Metadata.Name)
		st := &GatewayStatus{Namespace: gw.Metadata.Namespace, Name: gw.Metadata.Name, Generation: gw.Metadata.Generation}
		t.gateways[key] = &gatewayState{gw: gw, status: st}
		programmed := 0
		for _, l := range gw.Spec.Listeners {
			ls := &ListenerStatus{Name: l.Name, SupportedKinds: supportedKinds(l)}
			a := &attachable{gwKey: key, gwNS: gw.Metadata.Namespace, l: l, status: ls}
			reason, msg := t.programListener(gw, l, a)
			if reason == "" {
				programmed++
				a.accepted = true
				ls.Conditions = []Condition{
					cond("Accepted", true, "Accepted", ""),
					cond("Programmed", true, "Programmed", ""),
					cond("ResolvedRefs", true, "ResolvedRefs", ""),
					cond("Conflicted", false, "NoConflicts", ""),
				}
			} else {
				ls.Conditions = listenerFailure(reason, msg)
			}
			st.Listeners = append(st.Listeners, *ls)
			// Keep a pointer into the slice element for attached route counts.
			a.status = &st.Listeners[len(st.Listeners)-1]
			t.gwListeners[key] = append(t.gwListeners[key], a)
		}
		st.Conditions = []Condition{cond("Accepted", true, "Accepted", "")}
		if programmed > 0 {
			st.Conditions = append(st.Conditions, cond("Programmed", true, "Programmed", ""))
		} else {
			st.Conditions = append(st.Conditions, cond("Programmed", false, "Invalid", "no listener could be programmed"))
		}
		t.out.Status.Gateways = append(t.out.Status.Gateways, *st)
		t.gateways[key].status = &t.out.Status.Gateways[len(t.out.Status.Gateways)-1]
	}
	// Re-point listener statuses at their final location (the gateways
	// slice may have reallocated while appending).
	for i := range t.out.Status.Gateways {
		gs := &t.out.Status.Gateways[i]
		key := gatewayRefKey(gs.Namespace, gs.Name)
		t.gateways[key].status = gs
		for j, a := range t.gwListeners[key] {
			a.status = &gs.Listeners[j]
		}
	}
}

func listenerFailure(reason, msg string) []Condition {
	conds := []Condition{cond("Programmed", false, "Invalid", msg)}
	switch reason {
	case "UnsupportedProtocol":
		conds = append(conds, cond("Accepted", false, reason, msg))
	case "InvalidCertificateRef", "RefNotPermitted":
		conds = append(conds, cond("Accepted", true, "Accepted", ""), cond("ResolvedRefs", false, reason, msg))
	case "ProtocolConflict", "HostnameConflict":
		conds = append(conds, cond("Accepted", false, reason, msg), cond("Conflicted", true, reason, msg))
	}
	return conds
}

func supportedKinds(l listener) []string {
	if l.AllowedRoutes != nil && len(l.AllowedRoutes.Kinds) > 0 {
		var out []string
		for _, k := range l.AllowedRoutes.Kinds {
			if k.Kind == KindHTTPRoute || k.Kind == KindGRPCRoute {
				out = append(out, k.Kind)
			}
		}
		return out
	}
	return []string{KindHTTPRoute, KindGRPCRoute}
}

// programListener resolves a listener into a port socket, returning a
// failure reason (empty on success).
func (t *translator) programListener(gw gateway, l listener, a *attachable) (string, string) {
	if l.Protocol != "HTTP" && l.Protocol != "HTTPS" {
		return "UnsupportedProtocol", fmt.Sprintf("protocol %s is not supported (HTTP and HTTPS are)", l.Protocol)
	}
	var certs []config.Certificate
	if l.Protocol == "HTTPS" {
		if l.TLS != nil && deref(l.TLS.Mode, "Terminate") != "Terminate" {
			return "UnsupportedProtocol", "only TLS mode Terminate is supported"
		}
		if l.TLS == nil || len(l.TLS.CertificateRefs) == 0 {
			return "InvalidCertificateRef", "HTTPS listeners need tls.certificateRefs"
		}
		for _, ref := range l.TLS.CertificateRefs {
			cert, reason, msg := t.resolveCert(gw, ref)
			if reason != "" {
				return reason, msg
			}
			certs = append(certs, cert)
		}
	}
	pl := t.ports[l.Port]
	if pl == nil {
		pl = &portListener{name: fmt.Sprintf("k8s/port/%d", l.Port), port: l.Port, protocol: l.Protocol}
		t.ports[l.Port] = pl
	} else if pl.protocol != l.Protocol {
		return "ProtocolConflict", fmt.Sprintf("port %d is already used with protocol %s", l.Port, pl.protocol)
	}
	// Two listeners on one port must differ by hostname.
	host := deref(l.Hostname, "")
	for _, others := range t.gwListeners {
		for _, o := range others {
			if o.accepted && o.l.Port == l.Port && deref(o.l.Hostname, "") == host {
				return "HostnameConflict", fmt.Sprintf("another listener already serves port %d for hostname %q", l.Port, host)
			}
		}
	}
	for _, c := range certs {
		if !slices.Contains(pl.certs, c) {
			pl.certs = append(pl.certs, c)
		}
	}
	a.merged = pl
	return "", ""
}

func (t *translator) resolveCert(gw gateway, ref objectRef) (config.Certificate, string, string) {
	if deref(ref.Group, "") != "" || deref(ref.Kind, KindSecret) != KindSecret {
		return config.Certificate{}, "InvalidCertificateRef", "certificateRefs must reference core Secrets"
	}
	ns := deref(ref.Namespace, gw.Metadata.Namespace)
	if ns != gw.Metadata.Namespace && !t.granted(Group, KindGateway, gw.Metadata.Namespace, "", KindSecret, ns, ref.Name) {
		return config.Certificate{}, "RefNotPermitted", fmt.Sprintf("no ReferenceGrant allows Secret %s/%s", ns, ref.Name)
	}
	o, ok := t.in.Secrets[ns+"/"+ref.Name]
	if !ok {
		return config.Certificate{}, "InvalidCertificateRef", fmt.Sprintf("Secret %s/%s not found", ns, ref.Name)
	}
	var s secret
	if err := json.Unmarshal(o.Raw, &s); err != nil {
		return config.Certificate{}, "InvalidCertificateRef", err.Error()
	}
	crt, key := s.Data["tls.crt"], s.Data["tls.key"]
	if _, err := tls.X509KeyPair(crt, key); err != nil {
		return config.Certificate{}, "InvalidCertificateRef", fmt.Sprintf("Secret %s/%s: %v", ns, ref.Name, err)
	}
	return config.Certificate{CertPEM: string(crt), KeyPEM: string(key)}, "", ""
}

// granted reports whether a ReferenceGrant in toNS allows the reference.
func (t *translator) granted(fromGroup, fromKind, fromNS, toGroup, toKind, toNS, toName string) bool {
	for _, o := range t.in.ReferenceGrants {
		if o.Namespace != toNS {
			continue
		}
		var g referenceGrant
		if json.Unmarshal(o.Raw, &g) != nil {
			continue
		}
		from := slices.ContainsFunc(g.Spec.From, func(f struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
		}) bool {
			return f.Group == fromGroup && f.Kind == fromKind && f.Namespace == fromNS
		})
		to := slices.ContainsFunc(g.Spec.To, func(to struct {
			Group string  `json:"group"`
			Kind  string  `json:"kind"`
			Name  *string `json:"name"`
		}) bool {
			return to.Group == toGroup && to.Kind == toKind && (to.Name == nil || *to.Name == toName)
		})
		if from && to {
			return true
		}
	}
	return false
}

// --- PrismaPolicies ---

type policyTarget struct {
	ref    targetRef
	key    string
	policy string
}

type prismaPolicySpec struct {
	TargetRefs    []targetRef `yaml:"targetRefs"`
	config.Policy `yaml:",inline"`
}

func (t *translator) translatePolicies() {
	t.policyByTarget = map[string][]string{}
	t.policyTargets = map[string][]policyTarget{}
	type entry struct {
		meta objectMeta
		spec prismaPolicySpec
		err  error
	}
	var entries []entry
	for _, o := range t.in.Policies {
		var obj struct {
			Metadata objectMeta      `json:"metadata"`
			Spec     json.RawMessage `json:"spec"`
		}
		e := entry{}
		if err := json.Unmarshal(o.Raw, &obj); err != nil {
			continue
		}
		e.meta = obj.Metadata
		// JSON is YAML: decoding with the YAML tags gives "30s" durations and
		// "1MiB" sizes the same meaning as in the config file.
		dec := yaml.NewDecoder(strings.NewReader(string(obj.Spec)))
		dec.KnownFields(true)
		if err := dec.Decode(&e.spec); err != nil {
			e.err = fmt.Errorf("invalid spec: %w", err)
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b entry) int { return olderFirst(a.meta, b.meta) })
	for _, e := range entries {
		name := "k8s/policy/" + e.meta.Namespace + "/" + e.meta.Name
		ps := PolicyStatus{Namespace: e.meta.Namespace, Name: e.meta.Name, Generation: e.meta.Generation}
		err := e.err
		if err == nil && len(e.spec.TargetRefs) == 0 {
			err = fmt.Errorf("spec.targetRefs is required")
		}
		if err == nil && t.in.Base != nil {
			err = t.in.Base.ValidatePolicy(name, &e.spec.Policy)
		}
		if err != nil {
			for _, ref := range e.spec.TargetRefs {
				ps.Ancestors = append(ps.Ancestors, AncestorStatus{Ref: ref, Conditions: []Condition{cond("Accepted", false, "Invalid", err.Error())}})
			}
			if len(e.spec.TargetRefs) == 0 {
				ps.Ancestors = append(ps.Ancestors, AncestorStatus{Conditions: []Condition{cond("Accepted", false, "Invalid", err.Error())}})
			}
			t.out.Status.Policies = append(t.out.Status.Policies, ps)
			continue
		}
		t.out.Policies[name] = e.spec.Policy
		for _, ref := range e.spec.TargetRefs {
			key := ref.Kind + "/" + e.meta.Namespace + "/" + ref.Name
			t.policyByTarget[key] = append(t.policyByTarget[key], name)
			t.policyTargets[name] = append(t.policyTargets[name], policyTarget{ref: ref, key: key, policy: name})
		}
		t.out.Status.Policies = append(t.out.Status.Policies, ps)
	}
}

// --- Routes ---

type routeObj struct {
	kind  string
	meta  objectMeta
	refs  []parentRef
	hosts []string
	http  *httpRoute
	grpc  *grpcRoute
}

type attachment struct {
	gwKey  string
	merged *portListener
	hosts  []string // nil: any host
}

func (t *translator) translateRoutes() {
	var routes []routeObj
	for _, o := range t.in.HTTPRoutes {
		var r httpRoute
		if json.Unmarshal(o.Raw, &r) == nil {
			routes = append(routes, routeObj{kind: KindHTTPRoute, meta: r.Metadata, refs: r.Spec.ParentRefs, hosts: r.Spec.Hostnames, http: &r})
		}
	}
	for _, o := range t.in.GRPCRoutes {
		var r grpcRoute
		if json.Unmarshal(o.Raw, &r) == nil {
			routes = append(routes, routeObj{kind: KindGRPCRoute, meta: r.Metadata, refs: r.Spec.ParentRefs, hosts: r.Spec.Hostnames, grpc: &r})
		}
	}
	// Gateway API precedence ties: oldest first, then namespace/name.
	slices.SortFunc(routes, func(a, b routeObj) int { return olderFirst(a.meta, b.meta) })
	for _, r := range routes {
		t.route(r)
	}
}

func (t *translator) route(r routeObj) {
	rs := RouteStatus{Kind: r.kind, Namespace: r.meta.Namespace, Name: r.meta.Name, Generation: r.meta.Generation}
	var attachments []attachment
	for _, pr := range r.refs {
		if deref(pr.Group, Group) != Group || deref(pr.Kind, KindGateway) != KindGateway {
			continue // not a Gateway parent: not ours to report
		}
		ns := deref(pr.Namespace, r.meta.Namespace)
		key := gatewayRefKey(ns, pr.Name)
		if _, ok := t.gateways[key]; !ok {
			continue // a Gateway of another implementation (or missing)
		}
		atts, reason, msg := t.attach(r, key, pr)
		ps := ParentStatus{Ref: normalizeParent(pr, ns), ControllerName: t.in.ControllerName}
		if len(atts) == 0 {
			ps.Conditions = []Condition{cond("Accepted", false, reason, msg)}
		} else {
			ps.Conditions = []Condition{cond("Accepted", true, "Accepted", "")}
			attachments = append(attachments, atts...)
		}
		rs.Parents = append(rs.Parents, ps)
	}
	if len(rs.Parents) == 0 {
		return
	}
	var problems []string
	resolved := true
	if len(attachments) > 0 {
		var rules []ruleOut
		var err error
		if r.http != nil {
			rules, resolved, problems, err = t.httpRules(r)
		} else {
			rules, resolved, problems, err = t.grpcRules(r)
		}
		if err != nil {
			for i := range rs.Parents {
				rs.Parents[i].Conditions = []Condition{cond("Accepted", false, "UnsupportedValue", err.Error())}
			}
			t.out.Status.Routes = append(t.out.Status.Routes, rs)
			return
		}
		t.emit(r, rules, attachments)
	}
	for i := range rs.Parents {
		if resolved {
			rs.Parents[i].Conditions = append(rs.Parents[i].Conditions, cond("ResolvedRefs", true, "ResolvedRefs", ""))
		} else {
			reason := "BackendNotFound"
			for _, p := range problems {
				if strings.Contains(p, "ReferenceGrant") {
					reason = "RefNotPermitted"
				}
			}
			rs.Parents[i].Conditions = append(rs.Parents[i].Conditions, cond("ResolvedRefs", false, reason, strings.Join(problems, "; ")))
		}
	}
	t.out.Status.Routes = append(t.out.Status.Routes, rs)
}

func normalizeParent(pr parentRef, ns string) parentRef {
	g, k := Group, KindGateway
	return parentRef{Group: &g, Kind: &k, Namespace: &ns, Name: pr.Name, SectionName: pr.SectionName, Port: pr.Port}
}

// attach finds the listeners of one Gateway a route may bind to.
func (t *translator) attach(r routeObj, gwKey string, pr parentRef) ([]attachment, string, string) {
	var out []attachment
	reason, msg := "NoMatchingParent", "no listener matches the parentRef"
	for _, a := range t.gwListeners[gwKey] {
		if pr.SectionName != nil && *pr.SectionName != a.l.Name {
			continue
		}
		if pr.Port != nil && *pr.Port != a.l.Port {
			continue
		}
		if !a.accepted {
			reason, msg = "NoMatchingParent", "listener "+a.l.Name+" is not programmed"
			continue
		}
		if !slices.Contains(supportedKinds(a.l), r.kind) {
			reason, msg = "NotAllowedByListeners", "listener "+a.l.Name+" does not allow "+r.kind
			continue
		}
		from := "Same"
		if a.l.AllowedRoutes != nil && a.l.AllowedRoutes.Namespaces != nil {
			from = deref(a.l.AllowedRoutes.Namespaces.From, "Same")
		}
		if (from == "Same" && r.meta.Namespace != a.gwNS) || (from != "Same" && from != "All") {
			reason, msg = "NotAllowedByListeners", "listener "+a.l.Name+" does not allow routes from namespace "+r.meta.Namespace
			continue
		}
		hosts, ok := intersectHosts(deref(a.l.Hostname, ""), r.hosts)
		if !ok {
			reason, msg = "NoMatchingListenerHostname", "no route hostname matches listener "+a.l.Name
			continue
		}
		a.status.AttachedRoutes++
		out = append(out, attachment{gwKey: gwKey, merged: a.merged, hosts: hosts})
	}
	return out, reason, msg
}

// intersectHosts applies Gateway API hostname matching between a listener
// hostname ("" = any, "*.x.com" wildcard) and route hostnames.
func intersectHosts(listener string, route []string) ([]string, bool) {
	if listener == "" {
		return route, true
	}
	if len(route) == 0 {
		return []string{listener}, true
	}
	var out []string
	for _, h := range route {
		switch {
		case h == listener:
			out = append(out, h)
		case strings.HasPrefix(listener, "*.") && !strings.HasPrefix(h, "*.") && strings.HasSuffix(h, listener[1:]):
			out = append(out, h)
		case strings.HasPrefix(h, "*.") && !strings.HasPrefix(listener, "*.") && strings.HasSuffix(listener, h[1:]):
			out = append(out, listener)
		case strings.HasPrefix(h, "*.") && strings.HasPrefix(listener, "*."):
			if strings.HasSuffix(h, listener[1:]) {
				out = append(out, h)
			} else if strings.HasSuffix(listener, h[1:]) {
				out = append(out, listener)
			}
		}
	}
	return out, len(out) > 0
}

// ruleOut is one generated config route before attachment.
type ruleOut struct {
	route config.Route
	rule  int
	match int
}

func (t *translator) emit(r routeObj, rules []ruleOut, atts []attachment) {
	var use []string
	seen := map[string]bool{}
	addPolicies := func(key string) {
		for _, p := range t.policyByTarget[key] {
			if !seen[p] {
				seen[p] = true
				use = append(use, p)
			}
		}
	}
	for _, a := range atts {
		addPolicies(KindGateway + "/" + a.gwKey)
	}
	// Route policies come last so they override Gateway policies.
	addPolicies(r.kind + "/" + r.meta.Namespace + "/" + r.meta.Name)

	for ai, a := range atts {
		for _, ro := range rules {
			cr := ro.route
			cr.Name = fmt.Sprintf("k8s/%s/%s/%s/r%d/m%d/a%d", strings.ToLower(r.kind), r.meta.Namespace, r.meta.Name, ro.rule, ro.match, ai)
			cr.Listeners = []string{a.merged.name}
			cr.Match.Hosts = a.hosts
			cr.Use = append(slices.Clone(use), cr.Use...)
			t.out.Routes = append(t.out.Routes, cr)
		}
	}
}

func (t *translator) httpRules(r routeObj) ([]ruleOut, bool, []string, error) {
	var out []ruleOut
	resolved := true
	var problems []string
	for ri, rule := range r.http.Spec.Rules {
		base := config.Route{}
		if to := rule.Timeouts; to != nil && to.Request != nil {
			d, err := time.ParseDuration(*to.Request)
			if err != nil {
				return nil, false, nil, fmt.Errorf("rules[%d].timeouts.request: %v", ri, err)
			}
			base.Timeout = &d
		}
		matches := rule.Matches
		if len(matches) == 0 {
			matches = append(matches, struct {
				Path *struct {
					Type  *string `json:"type"`
					Value *string `json:"value"`
				} `json:"path"`
				Headers     []typedMatch `json:"headers"`
				QueryParams []typedMatch `json:"queryParams"`
				Method      *string      `json:"method"`
			}{})
		}
		for mi, m := range matches {
			cr := base
			pathType, pathValue := "PathPrefix", "/"
			if m.Path != nil {
				pathType, pathValue = deref(m.Path.Type, "PathPrefix"), deref(m.Path.Value, "/")
			}
			switch pathType {
			case "Exact":
				cr.Match.Path = pathValue
			case "PathPrefix":
				cr.Match.PathPrefix = pathValue
			case "RegularExpression":
				if _, err := regexp.Compile(pathValue); err != nil {
					return nil, false, nil, fmt.Errorf("rules[%d].matches[%d].path: %v", ri, mi, err)
				}
				cr.Match.PathRegex = pathValue
			default:
				return nil, false, nil, fmt.Errorf("rules[%d].matches[%d]: unsupported path type %s", ri, mi, pathType)
			}
			if err := exactMatches(m.Headers, &cr.Match.Headers); err != nil {
				return nil, false, nil, fmt.Errorf("rules[%d].matches[%d].headers: %v", ri, mi, err)
			}
			if err := exactMatches(m.QueryParams, &cr.Match.QueryParams); err != nil {
				return nil, false, nil, fmt.Errorf("rules[%d].matches[%d].queryParams: %v", ri, mi, err)
			}
			if m.Method != nil {
				cr.Match.Methods = []string{*m.Method}
			}
			if err := t.applyFilters(&cr, rule.Filters, r, pathType, pathValue); err != nil {
				return nil, false, nil, fmt.Errorf("rules[%d]: %v", ri, err)
			}
			if cr.Redirect == nil {
				ok, probs := t.backends(&cr, rule.BackendRefs, r, "http1")
				resolved = resolved && ok
				problems = append(problems, probs...)
			}
			out = append(out, ruleOut{route: cr, rule: ri, match: mi})
		}
	}
	return out, resolved, dedupe(problems), nil
}

func (t *translator) grpcRules(r routeObj) ([]ruleOut, bool, []string, error) {
	var out []ruleOut
	resolved := true
	var problems []string
	for ri, rule := range r.grpc.Spec.Rules {
		type gm = struct {
			Method *struct {
				Type    *string `json:"type"`
				Service *string `json:"service"`
				Method  *string `json:"method"`
			} `json:"method"`
			Headers []typedMatch `json:"headers"`
		}
		matches := rule.Matches
		if len(matches) == 0 {
			matches = []gm{{}}
		}
		for mi, m := range matches {
			cr := config.Route{Type: "grpc"}
			if mm := m.Method; mm != nil {
				svc, meth := deref(mm.Service, ""), deref(mm.Method, "")
				switch deref(mm.Type, "Exact") {
				case "Exact":
					switch {
					case svc != "" && meth != "":
						cr.Match.GRPCService, cr.Match.GRPCMethod = svc, meth
					case svc != "":
						cr.Match.GRPCService = svc
					case meth != "":
						cr.Match.PathRegex = "^/[^/]+/" + regexp.QuoteMeta(meth) + "$"
					}
				case "RegularExpression":
					sre, mre := "[^/]+", "[^/]+"
					if svc != "" {
						sre = svc
					}
					if meth != "" {
						mre = meth
					}
					cr.Match.PathRegex = "^/(" + sre + ")/(" + mre + ")$"
					if _, err := regexp.Compile(cr.Match.PathRegex); err != nil {
						return nil, false, nil, fmt.Errorf("rules[%d].matches[%d].method: %v", ri, mi, err)
					}
				default:
					return nil, false, nil, fmt.Errorf("rules[%d].matches[%d]: unsupported method match type", ri, mi)
				}
			}
			if cr.Match.GRPCService == "" && cr.Match.PathRegex == "" {
				cr.Match.PathPrefix = "/"
			}
			if err := exactMatches(m.Headers, &cr.Match.Headers); err != nil {
				return nil, false, nil, fmt.Errorf("rules[%d].matches[%d].headers: %v", ri, mi, err)
			}
			if err := t.applyFilters(&cr, rule.Filters, r, "", ""); err != nil {
				return nil, false, nil, fmt.Errorf("rules[%d]: %v", ri, err)
			}
			ok, probs := t.backends(&cr, rule.BackendRefs, r, "h2c")
			resolved = resolved && ok
			problems = append(problems, probs...)
			out = append(out, ruleOut{route: cr, rule: ri, match: mi})
		}
	}
	return out, resolved, dedupe(problems), nil
}

func exactMatches(ms []typedMatch, dst *map[string]string) error {
	for _, m := range ms {
		if deref(m.Type, "Exact") != "Exact" {
			return fmt.Errorf("only Exact matches are supported")
		}
		if *dst == nil {
			*dst = map[string]string{}
		}
		(*dst)[m.Name] = m.Value
	}
	return nil
}

func (t *translator) applyFilters(cr *config.Route, filters []routeFilter, r routeObj, pathType, pathValue string) error {
	for _, f := range filters {
		switch f.Type {
		case "RequestHeaderModifier", "ResponseHeaderModifier":
			if cr.Headers == nil {
				cr.Headers = &config.Headers{}
			}
			mod, ops := f.RequestHeaderModifier, &cr.Headers.Request
			if f.Type == "ResponseHeaderModifier" {
				mod, ops = f.ResponseHeaderModifier, &cr.Headers.Response
			}
			if mod == nil {
				continue
			}
			for _, kv := range mod.Set {
				if ops.Set == nil {
					ops.Set = map[string]string{}
				}
				ops.Set[kv.Name] = kv.Value
			}
			for _, kv := range mod.Add {
				if ops.Add == nil {
					ops.Add = map[string]string{}
				}
				ops.Add[kv.Name] = kv.Value
			}
			ops.Remove = append(ops.Remove, mod.Remove...)
		case "RequestRedirect":
			rr := f.RequestRedirect
			if rr == nil {
				continue
			}
			red := &config.Redirect{Status: deref(rr.StatusCode, 302), Scheme: deref(rr.Scheme, "")}
			if rr.Hostname != nil {
				red.Host = *rr.Hostname
			}
			if rr.Port != nil {
				if red.Host == "" {
					return fmt.Errorf("requestRedirect.port without hostname is not supported")
				}
				red.Host += ":" + strconv.Itoa(*rr.Port)
			}
			if p := rr.Path; p != nil {
				switch p.Type {
				case "ReplaceFullPath":
					red.Path = deref(p.ReplaceFullPath, "/")
				case "ReplacePrefixMatch":
					if pathType != "PathPrefix" {
						return fmt.Errorf("ReplacePrefixMatch requires a PathPrefix match")
					}
					red.PathPrefixReplace = deref(p.ReplacePrefixMatch, "/")
				}
			}
			cr.Redirect = red
		case "URLRewrite":
			rw := f.URLRewrite
			if rw == nil {
				continue
			}
			cr.Rewrite = &config.Rewrite{Host: deref(rw.Hostname, "")}
			if p := rw.Path; p != nil {
				switch p.Type {
				case "ReplaceFullPath":
					cr.Rewrite.PathRegex, cr.Rewrite.Replacement = "^.*$", deref(p.ReplaceFullPath, "/")
				case "ReplacePrefixMatch":
					if pathType != "PathPrefix" {
						return fmt.Errorf("ReplacePrefixMatch requires a PathPrefix match")
					}
					cr.Rewrite.StripPrefix, cr.Rewrite.ReplacePrefix = pathValue, deref(p.ReplacePrefixMatch, "/")
				}
			}
		case "RequestMirror":
			rm := f.RequestMirror
			if rm == nil || cr.Mirror != nil {
				continue // one mirror per route
			}
			proto := "http1"
			if r.grpc != nil {
				proto = "h2c"
			}
			name, prob := t.cluster(rm.BackendRef, r, proto)
			if prob != "" {
				return fmt.Errorf("requestMirror: %s", prob)
			}
			cr.Mirror = &config.Mirror{Cluster: name, Percent: float64(deref(rm.Percent, 100))}
		default:
			return fmt.Errorf("filter type %s is not supported", f.Type)
		}
	}
	return nil
}

// backends resolves backendRefs into clusters. Invalid references are
// dropped and reported; with no usable backend the route answers 500, as
// the Gateway API requires.
func (t *translator) backends(cr *config.Route, refs []backendRef, r routeObj, proto string) (bool, []string) {
	var problems []string
	for _, ref := range refs {
		w := deref(ref.Weight, 1)
		if w == 0 {
			continue
		}
		name, prob := t.cluster(ref, r, proto)
		if prob != "" {
			problems = append(problems, prob)
			continue
		}
		cr.Backends = append(cr.Backends, config.Backend{Cluster: name, Weight: w})
	}
	if len(cr.Backends) == 0 {
		cr.DirectResponse = &config.DirectResponse{Status: 500, Body: "no valid backend for this route\n"}
	}
	return len(problems) == 0, problems
}

func (t *translator) cluster(ref backendRef, r routeObj, proto string) (string, string) {
	if deref(ref.Group, "") != "" || deref(ref.Kind, KindService) != KindService {
		return "", fmt.Sprintf("backend %s: only core Services are supported", ref.Name)
	}
	if ref.Port == nil {
		return "", fmt.Sprintf("backend %s: port is required", ref.Name)
	}
	ns := deref(ref.Namespace, r.meta.Namespace)
	if ns != r.meta.Namespace && !t.granted(Group, r.kind, r.meta.Namespace, "", KindService, ns, ref.Name) {
		return "", fmt.Sprintf("backend %s/%s: no ReferenceGrant allows this reference", ns, ref.Name)
	}
	o, ok := t.in.Services[ns+"/"+ref.Name]
	if !ok {
		return "", fmt.Sprintf("Service %s/%s not found", ns, ref.Name)
	}
	var svc service
	if err := json.Unmarshal(o.Raw, &svc); err != nil {
		return "", err.Error()
	}
	idx := slices.IndexFunc(svc.Spec.Ports, func(p struct {
		Name        string  `json:"name"`
		Port        int     `json:"port"`
		TargetPort  any     `json:"targetPort"`
		AppProtocol *string `json:"appProtocol"`
	}) bool {
		return p.Port == *ref.Port
	})
	if idx < 0 {
		return "", fmt.Sprintf("Service %s/%s has no port %d", ns, ref.Name, *ref.Port)
	}
	port := svc.Spec.Ports[idx]
	switch deref(port.AppProtocol, "") {
	case "kubernetes.io/h2c", "h2c", "grpc":
		proto = "h2c"
	}
	name := fmt.Sprintf("k8s/svc/%s/%s/%d", ns, ref.Name, *ref.Port)
	if proto == "h2c" {
		name += "/h2c"
	}
	if _, exists := t.clusters[name]; !exists {
		kd := t.in.Discovery
		kd.Service, kd.Namespace, kd.PortName = ref.Name, ns, port.Name
		d := &config.Discovery{Type: "kubernetes", Kubernetes: &kd}
		if tp, ok := port.TargetPort.(float64); ok && port.Name == "" {
			d.Port = int(tp)
		}
		t.clusters[name] = config.Cluster{
			Name:             name,
			Protocol:         proto,
			Discovery:        d,
			OutlierDetection: &config.OutlierDetection{},
		}
	}
	return name, ""
}

func (t *translator) finish() {
	for _, pl := range t.ports {
		l := config.Listener{Name: pl.name, Address: ":" + strconv.Itoa(pl.port), Protocols: []string{"http1", "h2c"}}
		if pl.protocol == "HTTPS" {
			l.Protocols = []string{"http1", "http2"}
			l.TLS = &config.ListenerTLS{Certificates: pl.certs}
		}
		t.out.Listeners = append(t.out.Listeners, l)
	}
	slices.SortFunc(t.out.Listeners, func(a, b config.Listener) int { return cmp.Compare(a.Name, b.Name) })
	for _, c := range t.clusters {
		t.out.Clusters = append(t.out.Clusters, c)
	}
	slices.SortFunc(t.out.Clusters, func(a, b config.Cluster) int { return cmp.Compare(a.Name, b.Name) })

	// Policy status: accepted per target if the target exists and is ours.
	for i := range t.out.Status.Policies {
		ps := &t.out.Status.Policies[i]
		name := "k8s/policy/" + ps.Namespace + "/" + ps.Name
		for _, pt := range t.policyTargets[name] {
			c := cond("Accepted", true, "Accepted", "")
			if !t.targetExists(pt) {
				c = cond("Accepted", false, "TargetNotFound", pt.ref.Kind+" "+pt.ref.Name+" not found or not handled by this controller")
			}
			ps.Ancestors = append(ps.Ancestors, AncestorStatus{Ref: pt.ref, Conditions: []Condition{c}})
		}
	}
}

func (t *translator) targetExists(pt policyTarget) bool {
	ns, name, _ := strings.Cut(strings.TrimPrefix(pt.key, pt.ref.Kind+"/"), "/")
	switch pt.ref.Kind {
	case KindGateway:
		_, ok := t.gateways[gatewayRefKey(ns, name)]
		return ok
	case KindHTTPRoute, KindGRPCRoute:
		return slices.ContainsFunc(t.out.Status.Routes, func(r RouteStatus) bool {
			return r.Kind == pt.ref.Kind && r.Namespace == ns && r.Name == name
		})
	}
	return false
}

func olderFirst(a, b objectMeta) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp); c != 0 {
		return c
	}
	return cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
}

func sortedObjects(m map[string]k8s.Object) []k8s.Object {
	out := make([]k8s.Object, 0, len(m))
	for _, o := range m {
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b k8s.Object) int { return cmp.Compare(a.Key(), b.Key()) })
	return out
}

func dedupe(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}
