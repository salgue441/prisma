package gatewayapi

// Condition mirrors metav1.Condition (lastTransitionTime is managed by the
// status writer, which preserves it when nothing else changed).
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func cond(typ string, ok bool, reason, msg string) Condition {
	s := "False"
	if ok {
		s = "True"
	}
	return Condition{Type: typ, Status: s, Reason: reason, Message: msg}
}

type Status struct {
	Classes  []ClassStatus
	Gateways []GatewayStatus
	Routes   []RouteStatus
	Policies []PolicyStatus
}

type ClassStatus struct {
	Name       string
	Generation int64
	Conditions []Condition
}

type GatewayStatus struct {
	Namespace, Name string
	Generation      int64
	Conditions      []Condition
	Listeners       []ListenerStatus
}

type ListenerStatus struct {
	Name           string
	SupportedKinds []string
	AttachedRoutes int
	Conditions     []Condition
}

type RouteStatus struct {
	Kind, Namespace, Name string
	Generation            int64
	Parents               []ParentStatus
}

type ParentStatus struct {
	Ref            parentRef
	ControllerName string
	Conditions     []Condition
}

type PolicyStatus struct {
	Namespace, Name string
	Generation      int64
	Ancestors       []AncestorStatus
}

type AncestorStatus struct {
	Ref        targetRef
	Conditions []Condition
}
