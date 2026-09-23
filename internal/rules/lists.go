package rules

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ParseLists reads named lists from JSON, {"name": [values...]}, where each
// list holds only strings or only numbers:
//
//	{"trusted_domains": ["gmail.com", "icloud.com"], "blocked_regions": [123, 204]}
//
// An empty list is a string list. Lists are normalized as StringList and
// NumberList do.
func ParseLists(b []byte) (Lists, error) {
	var raw map[string][]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("lists: %w", err)
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names) // report the first bad list deterministically
	lists := make(Lists, len(raw))
	for _, name := range names {
		var strs []string
		var nums []float64
		for _, v := range raw[name] {
			switch v := v.(type) {
			case string:
				strs = append(strs, v)
			case float64:
				nums = append(nums, v)
			default:
				return nil, fmt.Errorf("lists: list %q: %v is neither text nor a number", name, v)
			}
		}
		switch {
		case strs != nil && nums != nil:
			return nil, fmt.Errorf("lists: list %q mixes text and numbers", name)
		case nums != nil:
			lists[name] = NumberList(nums...)
		default:
			lists[name] = StringList(strs...)
		}
	}
	return lists, nil
}
