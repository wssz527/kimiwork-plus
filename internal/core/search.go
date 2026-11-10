package core

import (
	"golang.org/x/text/unicode/norm"
	"math"
	"sort"
	"strings"
	"unicode"
)

func SearchModels(presetID, query string) Document {
	return SearchCatalog(Preset(presetID), nil, query)
}
func SearchCatalog(preset map[string]any, discovery Document, query string) Document {
	originals := map[string]map[string]any{}
	discovered := map[string]map[string]any{}
	order := []string{}
	for _, v := range Rows(preset["models"]) {
		m := Object(v)
		id := String(m["id"])
		if id != "" {
			if originals[id] == nil {
				order = append(order, id)
			}
			originals[id] = m
		}
	}
	for _, v := range Rows(discovery["models"]) {
		m := Object(v)
		id := String(m["id"])
		if id != "" {
			if originals[id] == nil && discovered[id] == nil {
				order = append(order, id)
			}
			discovered[id] = m
		}
	}
	models := []any{}
	for _, id := range order {
		models = append(models, mergeCandidate(id, preset, originals[id], discovered[id], String(discovery["sourceURL"])))
	}
	q := normalizeSearch(query)
	if strings.TrimSpace(query) == "" {
		return Document{"query": query, "models": models[:min(len(models), 20)]}
	}
	type match struct {
		model any
		tier  int
		score float64
	}
	matches := []match{}
	for _, v := range models {
		m := Object(v)
		id := normalizeSearch(String(m["id"]))
		labels := []string{normalizeSearch(String(m["name"]))}
		for _, v := range Rows(m["aliases"]) {
			labels = append(labels, normalizeSearch(String(v)))
		}
		tier := 4
		score := math.Inf(1)
		if id == q {
			tier = 0
			score = 0
		} else {
			for _, s := range labels {
				if s == q && q != "" {
					tier = 1
					score = 0
				}
			}
			if tier > 1 && len(q) >= 2 {
				for _, s := range append(labels, id) {
					if strings.HasPrefix(s, q) {
						tier = 2
						score = 0
					}
				}
			}
			if tier > 2 && len([]rune(q)) >= 3 {
				qr := []rune(q)
				qr = qr[:min(len(qr), 64)]
				limit := max(1, min(4, (len(qr)+2)/3))
				for _, s := range append(labels, id) {
					if s == "" {
						continue
					}
					candidate := []rune(s)
					n := math.Inf(1)
					if strings.Contains(s, q) {
						n = 0.5 + math.Min(1, float64(max(0, len(candidate)-len(qr)))/1000)
					} else {
						candidate = candidate[:min(len(candidate), 128)]
						distance := editDistance(qr, candidate)
						if distance <= limit {
							n = 1 + float64(distance) + math.Abs(float64(len(candidate)-len(qr)))/1000
						}
					}
					score = math.Min(score, n)
				}
				if !math.IsInf(score, 1) {
					tier = 3
				}
			}
		}
		if tier < 4 {
			matches = append(matches, match{v, tier, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].tier != matches[j].tier {
			return matches[i].tier < matches[j].tier
		}
		return matches[i].score < matches[j].score
	})
	out := []any{}
	for _, m := range matches[:min(len(matches), 20)] {
		out = append(out, m.model)
	}
	return Document{"query": query, "models": out}
}
func normalizeSearch(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, norm.NFKC.String(s))
}
func editDistance(a, b []rune) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, x := range a {
		current := make([]int, len(b)+1)
		current[0] = i + 1
		for j, y := range b {
			cost := 0
			if x != y {
				cost = 1
			}
			current[j+1] = min(previous[j+1]+1, current[j]+1, previous[j]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}
func mergeCandidate(id string, preset, old, live map[string]any, url string) Document {
	out := Document{"id": id}
	sources := Document{}
	sourceFor := func(kind string, row map[string]any) Document {
		d := Document{"kind": kind}
		for _, v := range []any{preset["source"], row["source"]} {
			for _, k := range []string{"url", "checkedAt"} {
				if value := String(Object(v)[k]); value != "" {
					d[k] = value
				}
			}
		}
		if kind == "discovery" && url != "" {
			d["url"] = url
		}
		return d
	}
	for _, field := range []string{"name", "contextWindow", "maxOutputTokens", "tools", "reasoning", "inputModalities", "reasoningEfforts", "protocols", "description"} {
		kind := "preset"
		v, ok := live[field]
		if ok {
			kind = "discovery"
		} else {
			v, ok = old[field]
		}
		if ok {
			out[field] = v
			if kind == "discovery" {
				sources[kind] = sourceFor(kind, live)
			} else {
				sources[kind] = sourceFor(kind, old)
			}
			if field == "contextWindow" {
				out["metadataSource"] = sources[kind]
			}
		}
	}
	aliases := append(append([]any{}, Rows(old["aliases"])...), Rows(live["aliases"])...)
	aliases = uniqueStrings(aliases, len(aliases), len(aliases), func(s string) bool { return s != "" })
	if len(aliases) > 0 {
		out["aliases"] = aliases
	}
	for _, pair := range []struct {
		kind string
		row  map[string]any
	}{{"preset", old}, {"discovery", live}} {
		if len(Rows(pair.row["aliases"])) > 0 {
			sources[pair.kind] = sourceFor(pair.kind, pair.row)
		}
	}
	if len(sources) > 0 {
		out["sources"] = sources
	}
	if s := live["source"]; s != nil {
		out["source"] = s
	} else if s := old["source"]; s != nil {
		out["source"] = s
	}
	return out
}
