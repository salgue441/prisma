package config

// EffectivePolicy merges global policies, then the route's referenced policies,
// then the route's inline settings. Later layers override earlier ones; rate
// limits accumulate so a route can add limits on top of a shared baseline.
func (c *Config) EffectivePolicy(r *Route) Policy {
	var out Policy
	layer := func(p Policy) {
		if p.AuthN != nil {
			out.AuthN = p.AuthN
		}
		if p.AuthZ != nil {
			out.AuthZ = p.AuthZ
		}
		if p.Masking != nil {
			out.Masking = p.Masking
		}
		if p.Headers != nil {
			out.Headers = p.Headers
		}
		if p.GraphQL != nil {
			out.GraphQL = p.GraphQL
		}
		if p.CORS != nil {
			out.CORS = p.CORS
		}
		if p.Compression != nil {
			out.Compression = p.Compression
		}
		out.RateLimit = append(out.RateLimit, p.RateLimit...)
	}
	for _, name := range c.Global.Use {
		layer(c.Policies[name])
	}
	for _, name := range r.Use {
		layer(c.Policies[name])
	}
	layer(r.Policy)
	return out
}
