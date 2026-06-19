package github

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// AliasRule maps a workflow runs-on label to a runs-fleet spec, letting jobs
// that target an externally-defined custom label (e.g. an ARC scale-set label
// like "shared-8cpu-arm64") be served by runs-fleet without editing the
// workflow. The Spec is an ordinary runs-fleet label spec
// (cpu=8/arch=arm64/disk=100/...), so it is parsed by the same parser as the
// native runs-fleet marker.
//
// Match is compared against each runs-on label: as a literal string by default,
// or as a regular expression when Regex is true. For regex rules, Spec may
// reference capture groups with $1/${1} or ${name}, expanded per
// regexp.Regexp.ExpandString.
type AliasRule struct {
	Match string `json:"match"`
	Regex bool   `json:"regex,omitempty"`
	Spec  string `json:"spec"`
}

type compiledAliasRule struct {
	rule AliasRule
	re   *regexp.Regexp // nil for literal-match rules
}

// AliasResolver resolves a workflow label to a runs-fleet spec using an ordered
// list of rules (first match wins). The zero value (and a nil *AliasResolver)
// resolves nothing, so runs-fleet behaves exactly as if no aliases are
// configured.
type AliasResolver struct {
	rules []compiledAliasRule
}

// ParseAliasRules builds an AliasResolver from a JSON array of AliasRule. Blank
// input yields an empty resolver. It fails on malformed JSON, an empty match or
// spec, an uncompilable regex, or a literal rule whose spec does not parse — so
// misconfiguration surfaces at startup rather than when a job arrives.
func ParseAliasRules(jsonStr string) (*AliasResolver, error) {
	resolver := &AliasResolver{}
	trimmed := strings.TrimSpace(jsonStr)
	if trimmed == "" {
		return resolver, nil
	}

	var rules []AliasRule
	if err := json.Unmarshal([]byte(trimmed), &rules); err != nil {
		return nil, fmt.Errorf("invalid label alias JSON: %w", err)
	}

	resolver.rules = make([]compiledAliasRule, 0, len(rules))
	for i, rule := range rules {
		if strings.TrimSpace(rule.Match) == "" {
			return nil, fmt.Errorf("label alias rule %d: match is required", i)
		}
		if strings.TrimSpace(rule.Spec) == "" {
			return nil, fmt.Errorf("label alias rule %d (%q): spec is required", i, rule.Match)
		}

		compiled := compiledAliasRule{rule: rule}
		if rule.Regex {
			re, err := regexp.Compile(rule.Match)
			if err != nil {
				return nil, fmt.Errorf("label alias rule %d (%q): invalid regex: %w", i, rule.Match, err)
			}
			compiled.re = re
		} else if err := validateSpec(rule.Spec); err != nil {
			return nil, fmt.Errorf("label alias rule %d (%q): %w", i, rule.Match, err)
		}

		resolver.rules = append(resolver.rules, compiled)
	}

	return resolver, nil
}

// Resolve returns the runs-fleet spec for the first rule that matches label.
func (r *AliasResolver) Resolve(label string) (spec string, ok bool) {
	if r == nil {
		return "", false
	}
	for _, c := range r.rules {
		if c.re != nil {
			if idx := c.re.FindStringSubmatchIndex(label); idx != nil {
				return string(c.re.ExpandString(nil, c.rule.Spec, label, idx)), true
			}
			continue
		}
		if label == c.rule.Match {
			return c.rule.Spec, true
		}
	}
	return "", false
}

// Len reports how many alias rules are configured.
func (r *AliasResolver) Len() int {
	if r == nil {
		return 0
	}
	return len(r.rules)
}

// validateSpec checks that a literal spec parses and resolves to at least one
// instance type, so typos (cpu=abc, disk out of range) and impossible specs
// (unknown family, no matching instance type) fail at config load rather than
// when the first matching job arrives. Specs with regex capture placeholders
// cannot be validated until expanded, so they are skipped.
func validateSpec(spec string) error {
	cfg := &JobConfig{Spot: true}
	if err := parseLabelParts(cfg, strings.Split(spec, "/")); err != nil {
		return fmt.Errorf("invalid spec %q: %w", spec, err)
	}
	if err := ResolveFlexibleSpec(cfg); err != nil {
		return fmt.Errorf("invalid spec %q: %w", spec, err)
	}
	return nil
}
