package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// normalizeModel turns a requested model into the name used for matching:
// Gemini "models/" prefixes and CPA thinking suffixes such as
// "gemini-2.5-pro(8192)" are stripped, and case is folded.
func normalizeModel(model string) string {
	m := strings.TrimSpace(model)
	m = strings.TrimPrefix(m, "models/")
	if strings.HasSuffix(m, ")") {
		if open := strings.LastIndex(m, "("); open > 0 {
			m = m[:open]
		}
	}
	return strings.ToLower(strings.TrimSpace(m))
}

// normalizePatterns trims, lower-cases, de-duplicates and sorts patterns.
func normalizePatterns(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, p := range list {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// globMatch matches pattern against s where '*' matches any run of characters,
// including '/', so "anthropic/*" and "gpt-5*" behave as people expect.
func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		idx := strings.Index(s, part)
		if idx < 0 {
			return false
		}
		s = s[idx+len(part):]
	}
	return strings.HasSuffix(s, last)
}

func modelAllowed(patterns []string, model string) bool {
	m := normalizeModel(model)
	if m == "" {
		return false
	}
	for _, p := range patterns {
		if globMatch(p, m) {
			return true
		}
	}
	return false
}

// claudeCloakPrefix mirrors CPA's Anthropic model listing: non-Claude IDs are
// shown to Claude clients as this prefix plus the real ID reversed, and CPA
// reverses it again before routing a request.
const claudeCloakPrefix = "claude-fable-5-dd-"

func uncloakClaudeID(id string) string {
	encoded, ok := strings.CutPrefix(id, claudeCloakPrefix)
	if !ok || encoded == "" {
		return id
	}
	r := []rune(encoded)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// modelCatalog is a parsed /v1/models style response. OpenAI, Claude and Grok
// list data[].id; Codex lists models[].slug; Gemini lists models[].name.
type modelCatalog struct {
	fields  map[string]json.RawMessage
	list    string
	entries []json.RawMessage
	ids     []string // real model IDs, used for matching
	raw     []string // IDs exactly as listed, used for pagination fields
}

func parseModelCatalog(body []byte) (modelCatalog, bool) {
	var c modelCatalog
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &c.fields) != nil {
		return c, false
	}
	for _, list := range []string{"data", "models"} {
		if raw, ok := c.fields[list]; ok {
			if json.Unmarshal(raw, &c.entries) != nil {
				return c, false
			}
			c.list = list
			break
		}
	}
	if c.list == "" {
		return c, false
	}
	c.ids = make([]string, len(c.entries))
	c.raw = make([]string, len(c.entries))
	for i, entry := range c.entries {
		var m struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if json.Unmarshal(entry, &m) != nil {
			return c, false
		}
		id := m.ID
		if id == "" {
			id = m.Slug
		}
		if id == "" {
			id = strings.TrimPrefix(m.Name, "models/")
		}
		if id == "" {
			return c, false
		}
		c.raw[i] = id
		c.ids[i] = uncloakClaudeID(id)
	}
	return c, true
}

// filter keeps entries whose keep flag is set and re-encodes the catalog.
// Claude-style first_id/last_id pagination fields are kept consistent.
func (c modelCatalog) filter(keep func(id string) bool) ([]byte, int) {
	kept := make([]json.RawMessage, 0, len(c.entries))
	var keptIDs []string
	for i, entry := range c.entries {
		if keep(c.ids[i]) {
			kept = append(kept, entry)
			keptIDs = append(keptIDs, c.raw[i])
		}
	}
	fields := make(map[string]json.RawMessage, len(c.fields))
	for k, v := range c.fields {
		fields[k] = v
	}
	rawList, _ := json.Marshal(kept)
	fields[c.list] = rawList
	if _, ok := fields["first_id"]; ok {
		fields["first_id"], fields["last_id"] = json.RawMessage("null"), json.RawMessage("null")
		if len(keptIDs) > 0 {
			fields["first_id"], _ = json.Marshal(keptIDs[0])
			fields["last_id"], _ = json.Marshal(keptIDs[len(keptIDs)-1])
		}
	}
	out, _ := json.Marshal(fields)
	return out, len(kept)
}
