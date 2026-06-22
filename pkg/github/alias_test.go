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
			json:      `[{"match":"gpu-large","spec":"cpu=16/arch=amd64"}]`,
			wantRules: 1,
		},
		{
			name:      "single regex rule",
			json:      `[{"match":"^ci-(\\d+)x-(amd64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`,
			wantRules: 1,
		},
		{
			name:      "regex with named captures",
			json:      `[{"match":"^builder-(?P<arch>amd64|arm64)-(?P<size>\\d+)$","regex":true,"spec":"cpu=${size}/arch=${arch}"}]`,
			wantRules: 1,
		},
		{
			name:      "many heterogeneous rules",
			json:      `[{"match":"^ci-(\\d+)x$","regex":true,"spec":"cpu=${1}/arch=arm64"},{"match":"bigmem","spec":"cpu=4+8/ram=32+64/family=r8g"},{"match":"legacy-amd64","spec":"cpu=2/arch=amd64/spot=false"},{"match":"^cache-(\\d+)g$","regex":true,"spec":"cpu=4/arch=arm64/disk=${1}"}]`,
			wantRules: 4,
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
		{name: "uncompilable regex", json: `[{"match":"build-(","regex":true,"spec":"cpu=2"}]`},
		{name: "literal spec with bad cpu", json: `[{"match":"x","spec":"cpu=abc"}]`},
		{name: "literal spec with out-of-range disk", json: `[{"match":"x","spec":"disk=999999"}]`},
		{name: "literal spec with out-of-range gen", json: `[{"match":"x","spec":"cpu=4/gen=99"}]`},
		{name: "literal spec with unknown family resolves to nothing", json: `[{"match":"x","spec":"family=nosuchfamily/arch=arm64"}]`},
		{name: "literal spec with impossible cpu/ram resolves to nothing", json: `[{"match":"x","spec":"cpu=2+2/ram=999+1000/arch=arm64"}]`},
		{name: "valid rule followed by invalid literal rule", json: `[{"match":"ok","spec":"cpu=2"},{"match":"bad","spec":"cpu=oops"}]`},
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
			name:      "literal match, simple spec",
			json:      `[{"match":"gpu-large","spec":"cpu=16/arch=amd64"}]`,
			label:     "gpu-large",
			wantSpec:  "cpu=16/arch=amd64",
			wantMatch: true,
		},
		{
			name:      "literal match, rich spec (range, family, disk)",
			json:      `[{"match":"bigmem","spec":"cpu=4+8/ram=32+64/family=r8g+m8g/disk=200"}]`,
			label:     "bigmem",
			wantSpec:  "cpu=4+8/ram=32+64/family=r8g+m8g/disk=200",
			wantMatch: true,
		},
		{
			name:      "literal match, on-demand override",
			json:      `[{"match":"release-runner","spec":"cpu=8/arch=arm64/spot=false"}]`,
			label:     "release-runner",
			wantSpec:  "cpu=8/arch=arm64/spot=false",
			wantMatch: true,
		},
		{
			name:      "literal no match is case sensitive",
			json:      `[{"match":"gpu-large","spec":"cpu=16/arch=amd64"}]`,
			label:     "GPU-Large",
			wantMatch: false,
		},
		{
			name:      "regex numeric capture, braced ${1}",
			json:      `[{"match":"^ci-(\\d+)x-(amd64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`,
			label:     "ci-8x-arm64",
			wantSpec:  "cpu=8+8/arch=arm64",
			wantMatch: true,
		},
		{
			name:      "regex numeric capture, unbraced $1",
			json:      `[{"match":"^node-(\\d+)$","regex":true,"spec":"cpu=$1/arch=amd64"}]`,
			label:     "node-4",
			wantSpec:  "cpu=4/arch=amd64",
			wantMatch: true,
		},
		{
			name:      "regex named captures",
			json:      `[{"match":"^builder-(?P<arch>amd64|arm64)-(?P<size>\\d+)$","regex":true,"spec":"cpu=${size}/arch=${arch}"}]`,
			label:     "builder-amd64-16",
			wantSpec:  "cpu=16/arch=amd64",
			wantMatch: true,
		},
		{
			name:      "regex capture drives disk size",
			json:      `[{"match":"^cache-(\\d+)g$","regex":true,"spec":"cpu=4/arch=arm64/disk=${1}"}]`,
			label:     "cache-150g",
			wantSpec:  "cpu=4/arch=arm64/disk=150",
			wantMatch: true,
		},
		{
			name:      "x64 synonym passed through verbatim by the engine",
			json:      `[{"match":"^pool-(x64|arm64)$","regex":true,"spec":"cpu=4/arch=${1}"}]`,
			label:     "pool-x64",
			wantSpec:  "cpu=4/arch=x64",
			wantMatch: true,
		},
		{
			name:      "regex no match against a github default label",
			json:      `[{"match":"^ci-(\\d+)x$","regex":true,"spec":"cpu=${1}/arch=arm64"}]`,
			label:     "self-hosted",
			wantMatch: false,
		},
		{
			name:      "unanchored regex matches a substring",
			json:      `[{"match":"gpu","regex":true,"spec":"cpu=8/arch=amd64"}]`,
			label:     "team-gpu-runner",
			wantSpec:  "cpu=8/arch=amd64",
			wantMatch: true,
		},
		{
			name:      "first matching rule wins",
			json:      `[{"match":"^build-.*","regex":true,"spec":"cpu=2/arch=arm64"},{"match":"build-heavy","spec":"cpu=16/arch=arm64"}]`,
			label:     "build-heavy",
			wantSpec:  "cpu=2/arch=arm64",
			wantMatch: true,
		},
		{
			name:      "later rule matches when earlier rules do not",
			json:      `[{"match":"gpu-large","spec":"cpu=16/arch=amd64"},{"match":"^ci-(\\d+)x$","regex":true,"spec":"cpu=${1}/arch=arm64"}]`,
			label:     "ci-2x",
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
