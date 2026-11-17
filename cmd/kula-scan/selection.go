package main

import (
	"fmt"
	"strings"
)

// selection is what -only and -skip choose. Both hold lowercased categories
// and finding IDs; an empty only selects everything. A registry check runs
// when any finding ID it declares is selected, and reports only the selected
// ones. A nil *selection selects everything.
type selection struct {
	only, skip         map[string]bool
	onlyList, skipList []string // canonical spelling (category, or the catalog's ID), in the order given
}

// newSelection parses the -only and -skip lists. A token that is neither a
// category nor a catalog ID is an error naming it and the categories.
func newSelection(only, skip string) (*selection, error) {
	sel := &selection{only: map[string]bool{}, skip: map[string]bool{}}
	for _, flag := range []struct {
		name string
		list string
		set  map[string]bool
		out  *[]string
	}{
		{"-only", only, sel.only, &sel.onlyList},
		{"-skip", skip, sel.skip, &sel.skipList},
	} {
		var unknown []string
		for _, token := range splitList(flag.list) {
			name, ok := canonicalToken(token)
			if !ok {
				unknown = append(unknown, fmt.Sprintf("%q", token))
				continue
			}
			flag.set[token] = true
			*flag.out = append(*flag.out, name)
		}
		if len(unknown) > 0 {
			return nil, fmt.Errorf("%s: unknown category or finding ID %s (categories: %s; -list shows the finding IDs)",
				flag.name, strings.Join(unknown, ", "), strings.Join(categories(), ", "))
		}
	}
	return sel, nil
}

// canonicalToken returns the spelling of a lowercased -only/-skip token: the
// category itself, or the catalog's spelling of a finding ID.
func canonicalToken(token string) (string, bool) {
	for _, c := range categories() {
		if c == token {
			return c, true
		}
	}
	if e, ok := lookupID(token); ok {
		return e.ID, true
	}
	return "", false
}

// selected reports whether the finding id is reported.
func (sel *selection) selected(id string) bool {
	if sel == nil {
		return true
	}
	category := catalogEntryFor(id).Category
	id = strings.ToLower(id)
	if len(sel.only) > 0 && !sel.only[category] && !sel.only[id] {
		return false
	}
	return !sel.skip[category] && !sel.skip[id]
}

// selectedIDs returns the IDs among ids that are selected.
func (sel *selection) selectedIDs(ids []string) []string {
	var out []string
	for _, id := range ids {
		if sel.selected(id) {
			out = append(out, id)
		}
	}
	return out
}

// parseIgnore parses -ignore: finding IDs whose result stays in the report
// but does not affect the exit status. It returns the lowercased set and the
// catalog spellings, or an error naming the IDs the catalog does not know.
func parseIgnore(list string) (map[string]bool, []string, error) {
	set := map[string]bool{}
	var names, unknown []string
	for _, token := range splitList(list) {
		e, ok := lookupID(token)
		if !ok {
			unknown = append(unknown, fmt.Sprintf("%q", token))
			continue
		}
		set[token] = true
		names = append(names, e.ID)
	}
	if len(unknown) > 0 {
		return nil, nil, fmt.Errorf("-ignore: unknown finding ID %s (-list shows them)", strings.Join(unknown, ", "))
	}
	return set, names, nil
}

// markIgnored flags the findings whose ID is in ignore (lowercased IDs).
func markIgnored(findings []Finding, ignore map[string]bool) {
	for i := range findings {
		if ignore[strings.ToLower(findings[i].ID)] {
			findings[i].Ignored = true
		}
	}
}

// splitList splits a comma list into lowercased, trimmed tokens, dropping
// empty ones and repeats and keeping the order given.
func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
