package config

import "strings"

// ParseRouteGroups parses a spec like
// "fast=gpt-4o-mini,gemini-2.5-flash;smart=gpt-4o,claude-3-5-sonnet" into a map
// of alias -> candidate models. Malformed entries are skipped.
func ParseRouteGroups(spec string) map[string][]string {
	groups := map[string][]string{}
	for _, group := range strings.Split(spec, ";") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		alias, list, ok := strings.Cut(group, "=")
		alias = strings.TrimSpace(alias)
		if !ok || alias == "" {
			continue
		}
		var models []string
		for _, m := range strings.Split(list, ",") {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
		if len(models) > 0 {
			groups[alias] = models
		}
	}
	return groups
}

// FormatRouteGroups serializes route groups for display and PATCH round-trips.
func FormatRouteGroups(groups map[string][]string) string {
	if len(groups) == 0 {
		return ""
	}
	parts := make([]string, 0, len(groups))
	for alias, models := range groups {
		if len(models) == 0 {
			continue
		}
		parts = append(parts, alias+"="+strings.Join(models, ","))
	}
	return strings.Join(parts, ";")
}
