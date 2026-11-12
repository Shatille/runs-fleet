package fleet

import (
	"slices"
	"strings"
	"testing"
)

func TestNewGenFamilies_ResolveByFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		family string
		arch   string
		gen    int
		want   string
		cpu    int
		ram    float64
	}{
		{"c8i", "amd64", 8, "c8i.xlarge", 4, 8},
		{"m8i", "amd64", 8, "m8i.xlarge", 4, 16},
		{"r8i", "amd64", 8, "r8i.xlarge", 4, 32},
		{"c8a", "amd64", 8, "c8a.xlarge", 4, 8},
		{"m8a", "amd64", 8, "m8a.xlarge", 4, 16},
		{"r8a", "amd64", 8, "r8a.xlarge", 4, 32},
		{"c9g", "arm64", 9, "c9g.xlarge", 4, 8},
		{"m9g", "arm64", 9, "m9g.xlarge", 4, 16},
	}
	for _, tt := range tests {
		t.Run(tt.family, func(t *testing.T) {
			t.Parallel()
			got := ResolveInstanceTypes(FlexibleSpec{CPUMin: 4, CPUMax: 4, Families: []string{tt.family}})
			if !slices.Equal(got, []string{tt.want}) {
				t.Fatalf("ResolveInstanceTypes(family=%s, cpu=4) = %v, want [%s]", tt.family, got, tt.want)
			}
			spec, _ := GetInstanceSpec(tt.want)
			if spec.CPU != tt.cpu || spec.RAM != tt.ram || spec.Arch != tt.arch || spec.Gen != tt.gen {
				t.Errorf("%s = %+v, want cpu=%d ram=%v arch=%s gen=%d", tt.want, spec, tt.cpu, tt.ram, tt.arch, tt.gen)
			}
		})
	}
}

func TestNewGenFamilies_GenLabelResolvesWithDefaultFamilies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		arch string
		gen  int
		want []string
	}{
		{"amd64", 8, []string{"c8i.xlarge", "c8a.xlarge", "m8i.xlarge", "m8a.xlarge"}},
		{"arm64", 9, []string{"c9g.xlarge", "m9g.xlarge"}},
	}
	for _, tt := range tests {
		got := ResolveInstanceTypes(FlexibleSpec{CPUMin: 4, CPUMax: 4, Arch: tt.arch, Gen: tt.gen, Families: DefaultFlexibleFamilies(tt.arch)})
		if !slices.Equal(got, tt.want) {
			t.Errorf("gen=%d arch=%s resolved %v, want %v", tt.gen, tt.arch, got, tt.want)
		}
	}
}

func TestNewGenFamilies_HighMemoryX86StaysOptIn(t *testing.T) {
	t.Parallel()

	for _, arch := range []string{"amd64", ""} {
		for _, fam := range DefaultFlexibleFamilies(arch) {
			if fam == "r8i" || fam == "r8a" {
				t.Errorf("DefaultFlexibleFamilies(%q) contains %s; x86 high-memory families are opt-in", arch, fam)
			}
		}
	}
}

func TestCatalog_NoMetalSizes(t *testing.T) {
	t.Parallel()

	for _, inst := range InstanceCatalog {
		if strings.Contains(inst.Type, "metal") {
			t.Errorf("catalog contains bare-metal type %q", inst.Type)
		}
	}
}

// An on-demand launch uses only the first resolved type, and with no arch label
// that type also decides the architecture, so the order among equal-sized types
// must not move when families are added. cpu=1 is not covered: only arm64 has
// 1-vCPU sizes, so an arch-less cpu=1 request resolves to arm64 first.
func TestResolveInstanceTypes_FirstTypeIsPinned(t *testing.T) {
	t.Parallel()

	sizes := map[int]string{2: "large", 4: "xlarge", 8: "2xlarge", 16: "4xlarge", 32: "8xlarge"}
	firstFamily := map[string]string{"amd64": "c6i", "arm64": "c7g", "": "c6i"}

	for arch, family := range firstFamily {
		for cpu, size := range sizes {
			got := ResolveInstanceTypes(FlexibleSpec{CPUMin: cpu, CPUMax: cpu * 2, Arch: arch, Families: DefaultFlexibleFamilies(arch)})
			want := family + "." + size
			if len(got) == 0 || got[0] != want {
				t.Errorf("arch=%q cpu=%d first type = %v, want %s", arch, cpu, got, want)
			}
		}
	}
}

func TestResolveInstanceTypes_Amd64FirstOnTies(t *testing.T) {
	t.Parallel()

	got := ResolveInstanceTypes(FlexibleSpec{CPUMin: 4, CPUMax: 8, Families: DefaultFlexibleFamilies("")})
	for i := 1; i < len(got); i++ {
		prev, _ := GetInstanceSpec(got[i-1])
		cur, _ := GetInstanceSpec(got[i])
		if prev.CPU == cur.CPU && prev.RAM == cur.RAM && prev.Arch == "arm64" && cur.Arch == "amd64" {
			t.Errorf("%s (arm64) sorts before equal-sized %s (amd64)", got[i-1], got[i])
		}
	}
}
