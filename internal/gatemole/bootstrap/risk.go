package bootstrap

import "strings"

func riskFor(values ...string) string {
	joined := strings.ToLower(strings.Join(values, "/"))
	switch {
	case containsAny(joined, "auth", "login", "password", "session", "token", "jwt", "oauth"):
		return "high"
	case containsAny(joined, "payment", "billing", "checkout", "stripe", "invoice"):
		return "high"
	case containsAny(joined, "admin", "role", "permission", "rbac"):
		return "high"
	case containsAny(joined, "secret", "key", "credential", "vault"):
		return "high"
	case containsAny(joined, "migration", "schema", "database", "db"):
		return "medium"
	case containsAny(joined, "api", "route", "handler", "controller"):
		return "medium"
	case containsAny(joined, "docs", "static", "css", "readme"):
		return "low"
	default:
		return "medium"
	}
}

func maxRisk(values ...string) string {
	rank := map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}
	out := "low"
	for _, value := range values {
		if rank[value] > rank[out] {
			out = value
		}
	}
	return out
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
