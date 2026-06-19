package github

import (
	"strings"
	"testing"
)

func TestParseAliasRules_Valid(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		wantRules int
	}{
		{
			name:      "blank input yields empty resolver",
			json:      "",
			wantRules: 0,
		},
		{
			name:      "whitespace-only input yields empty resolver",
			json:      "   \n  ",
			wantRules: 0,
		},
		{
			name:      "empty array yields empty resolver",
			json:      "[]",
			wantRules: 0,
		},
		{
			name:      "single literal rule",
			json:      `[{"match":"gpu-trainer","spec":"cpu=16/arch=amd64"}]`,
			wantRules: 1,
		},
		{
			name:      "single regex rule",
			json:      `[{"match":"^shared-(\\d+)cpu-(x64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`,
			wantRules: 1,
		},
		{
			name:      "multiple rules",
			json:      `[{"match":"^shared-(\\d+)cpu-(x64|arm64)$","regex":true,"spec":"cpu=${1}/arch=${2}"},{"match":"gpu","spec":"cpu=16/arch=amd64"}]`,
			wantRules: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver, err := ParseAliasRules(tt.json)
			if err != nil {
				t.Fatalf("ParseAliasRules() unexpected error: %v", err)
			}
			if got := resolver.Len(); got != tt.wantRules {
				t.Errorf("Len() = %d, want %d", got, tt.wantRules)
			}
		})
	}
}

func TestParseAliasRules_Invalid(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{name: "malformed json", json: `[{"match":`},
		{name: "not an array", json: `{"match":"x","spec":"cpu=2"}`},
		{name: "missing match", json: `[{"spec":"cpu=2"}]`},
		{name: "blank match", json: `[{"match":"  ","spec":"cpu=2"}]`},
		{name: "missing spec", json: `[{"match":"x"}]`},
		{name: "blank spec", json: `[{"match":"x","spec":"  "}]`},
		{name: "uncompilable regex", json: `[{"match":"shared-(","regex":true,"spec":"cpu=2"}]`},
		{name: "literal spec with bad cpu", json: `[{"match":"x","spec":"cpu=abc"}]`},
		{name: "literal spec with out-of-range disk", json: `[{"match":"x","spec":"disk=999999"}]`},
		{name: "literal spec with unknown family resolves to nothing", json: `[{"match":"x","spec":"family=nosuchfamily/arch=arm64"}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseAliasRules(tt.json); err == nil {
				t.Errorf("ParseAliasRules(%q) expected error, got nil", tt.json)
			}
		})
	}
}

func TestAliasResolver_Resolve(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		label     string
		wantSpec  string
		wantMatch bool
	}{
		{
			name:      "literal match",
			json:      `[{"match":"gpu-trainer","spec":"cpu=16/arch=amd64"}]`,
			label:     "gpu-trainer",
			wantSpec:  "cpu=16/arch=amd64",
			wantMatch: true,
		},
		{
			name:      "literal no match is case sensitive",
			json:      `[{"match":"gpu-trainer","spec":"cpu=16/arch=amd64"}]`,
			label:     "GPU-Trainer",
			wantMatch: false,
		},
		{
			name:      "regex expands captures",
			json:      `[{"match":"^shared-(\\d+)cpu-(x64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`,
			label:     "shared-8cpu-arm64",
			wantSpec:  "cpu=8+8/arch=arm64",
			wantMatch: true,
		},
		{
			name:      "regex expands x64 capture verbatim",
			json:      `[{"match":"^shared-(\\d+)cpu-(x64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`,
			label:     "shared-16cpu-x64",
			wantSpec:  "cpu=16+16/arch=x64",
			wantMatch: true,
		},
		{
			name:      "regex no match",
			json:      `[{"match":"^shared-(\\d+)cpu-(x64|arm64)$","regex":true,"spec":"cpu=${1}/arch=${2}"}]`,
			label:     "self-hosted",
			wantMatch: false,
		},
		{
			name:      "first matching rule wins",
			json:      `[{"match":"^shared-.*","regex":true,"spec":"cpu=2/arch=arm64"},{"match":"shared-8cpu-arm64","spec":"cpu=8/arch=arm64"}]`,
			label:     "shared-8cpu-arm64",
			wantSpec:  "cpu=2/arch=arm64",
			wantMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver, err := ParseAliasRules(tt.json)
			if err != nil {
				t.Fatalf("ParseAliasRules() unexpected error: %v", err)
			}
			gotSpec, gotMatch := resolver.Resolve(tt.label)
			if gotMatch != tt.wantMatch {
				t.Fatalf("Resolve(%q) match = %v, want %v", tt.label, gotMatch, tt.wantMatch)
			}
			if gotMatch && gotSpec != tt.wantSpec {
				t.Errorf("Resolve(%q) spec = %q, want %q", tt.label, gotSpec, tt.wantSpec)
			}
		})
	}
}

func TestAliasResolver_NilSafe(t *testing.T) {
	var resolver *AliasResolver
	if spec, ok := resolver.Resolve("anything"); ok || spec != "" {
		t.Errorf("nil resolver Resolve() = (%q, %v), want (\"\", false)", spec, ok)
	}
	if n := resolver.Len(); n != 0 {
		t.Errorf("nil resolver Len() = %d, want 0", n)
	}
}

func TestParseAliasRules_ErrorMentionsRuleIndex(t *testing.T) {
	_, err := ParseAliasRules(`[{"match":"ok","spec":"cpu=2"},{"match":"bad","spec":"cpu=oops"}]`)
	if err == nil {
		t.Fatal("expected error for invalid second rule")
	}
	if !strings.Contains(err.Error(), "rule 1") {
		t.Errorf("error %q should identify the offending rule index", err.Error())
	}
}
