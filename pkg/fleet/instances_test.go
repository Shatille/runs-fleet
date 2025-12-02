package fleet

import (
	"testing"
)

func TestGetInstanceSpec(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		wantFound    bool
		wantCPU      int
		wantRAM      float64
		wantArch     string
	}{
		{
			name:         "c7g.xlarge exists",
			instanceType: "c7g.xlarge",
			wantFound:    true,
			wantCPU:      4,
			wantRAM:      8,
			wantArch:     "arm64",
		},
		{
			name:         "t4g.medium exists",
			instanceType: "t4g.medium",
			wantFound:    true,
			wantCPU:      2,
			wantRAM:      4,
			wantArch:     "arm64",
		},
		{
			name:         "c6i.2xlarge exists",
			instanceType: "c6i.2xlarge",
			wantFound:    true,
			wantCPU:      8,
			wantRAM:      16,
			wantArch:     "amd64",
		},
		{
			name:         "unknown instance type",
			instanceType: "unknown.type",
			wantFound:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, found := GetInstanceSpec(tt.instanceType)
			if found != tt.wantFound {
				t.Errorf("GetInstanceSpec(%q) found = %v, want %v", tt.instanceType, found, tt.wantFound)
			}
			if tt.wantFound {
				if spec.CPU != tt.wantCPU {
					t.Errorf("GetInstanceSpec(%q) CPU = %d, want %d", tt.instanceType, spec.CPU, tt.wantCPU)
				}
				if spec.RAM != tt.wantRAM {
					t.Errorf("GetInstanceSpec(%q) RAM = %f, want %f", tt.instanceType, spec.RAM, tt.wantRAM)
				}
				if spec.Arch != tt.wantArch {
					t.Errorf("GetInstanceSpec(%q) Arch = %s, want %s", tt.instanceType, spec.Arch, tt.wantArch)
				}
			}
		})
	}
}

func TestResolveInstanceTypes(t *testing.T) {
	tests := []struct {
		name     string
		spec     FlexibleSpec
		wantLen  int
		contains []string // Instance types that should be in the result
		excludes []string // Instance types that should NOT be in the result
	}{
		{
			name: "4 CPU ARM64 c7g family",
			spec: FlexibleSpec{
				CPUMin:   4,
				CPUMax:   4,
				Arch:     "arm64",
				Families: []string{"c7g"},
			},
			wantLen:  1,
			contains: []string{"c7g.xlarge"},
			excludes: []string{"c7g.2xlarge", "c7g.large"},
		},
		{
			name: "4-8 CPU ARM64 c7g family",
			spec: FlexibleSpec{
				CPUMin:   4,
				CPUMax:   8,
				Arch:     "arm64",
				Families: []string{"c7g"},
			},
			wantLen:  2,
			contains: []string{"c7g.xlarge", "c7g.2xlarge"},
			excludes: []string{"c7g.large", "c7g.4xlarge"},
		},
		{
			name: "4+ CPU ARM64 multiple families",
			spec: FlexibleSpec{
				CPUMin:   4,
				CPUMax:   8,
				Arch:     "arm64",
				Families: []string{"c7g", "m7g"},
			},
			contains: []string{"c7g.xlarge", "c7g.2xlarge", "m7g.xlarge", "m7g.2xlarge"},
		},
		{
			name: "2 CPU amd64 t3 family",
			spec: FlexibleSpec{
				CPUMin:   2,
				CPUMax:   2,
				Arch:     "amd64",
				Families: []string{"t3"},
			},
			contains: []string{"t3.medium", "t3.small", "t3.micro", "t3.large"},
			excludes: []string{"t3.xlarge"},
		},
		{
			name: "RAM filter - minimum 16GB",
			spec: FlexibleSpec{
				CPUMin:   4,
				CPUMax:   8,
				RAMMin:   16,
				Arch:     "arm64",
				Families: []string{"m7g"},
			},
			contains: []string{"m7g.xlarge", "m7g.2xlarge"},
			excludes: []string{"m7g.large"},
		},
		{
			name: "RAM filter - 8-16GB range",
			spec: FlexibleSpec{
				CPUMin:   2,
				CPUMax:   4,
				RAMMin:   8,
				RAMMax:   16,
				Arch:     "arm64",
				Families: []string{"m7g"},
			},
			contains: []string{"m7g.large", "m7g.xlarge"},
			excludes: []string{"m7g.2xlarge"},
		},
		{
			name: "No family filter - uses all matching",
			spec: FlexibleSpec{
				CPUMin: 4,
				CPUMax: 4,
				Arch:   "arm64",
			},
			contains: []string{"c7g.xlarge", "m7g.xlarge", "t4g.xlarge"},
		},
		{
			name: "No matches - impossible spec",
			spec: FlexibleSpec{
				CPUMin:   100,
				CPUMax:   100,
				Arch:     "arm64",
				Families: []string{"t4g"},
			},
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ResolveInstanceTypes(tt.spec)

			if tt.wantLen > 0 && len(result) != tt.wantLen {
				t.Errorf("ResolveInstanceTypes() returned %d types, want %d", len(result), tt.wantLen)
			}

			for _, want := range tt.contains {
				found := false
				for _, got := range result {
					if got == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("ResolveInstanceTypes() missing expected type %q, got %v", want, result)
				}
			}

			for _, exclude := range tt.excludes {
				for _, got := range result {
					if got == exclude {
						t.Errorf("ResolveInstanceTypes() should not contain %q, got %v", exclude, result)
						break
					}
				}
			}
		})
	}
}

func TestResolveInstanceTypes_SortedByCPU(t *testing.T) {
	spec := FlexibleSpec{
		CPUMin:   2,
		CPUMax:   16,
		Arch:     "arm64",
		Families: []string{"c7g"},
	}

	result := ResolveInstanceTypes(spec)

	if len(result) < 2 {
		t.Fatalf("Expected at least 2 results, got %d", len(result))
	}

	// Verify sorted by CPU (ascending)
	for i := 1; i < len(result); i++ {
		prevSpec, _ := GetInstanceSpec(result[i-1])
		currSpec, _ := GetInstanceSpec(result[i])
		if prevSpec.CPU > currSpec.CPU {
			t.Errorf("Results not sorted by CPU: %s (%d CPU) before %s (%d CPU)",
				result[i-1], prevSpec.CPU, result[i], currSpec.CPU)
		}
	}
}

func TestDefaultFlexibleFamilies(t *testing.T) {
	arm64Families := DefaultFlexibleFamilies("arm64")
	if len(arm64Families) == 0 {
		t.Error("DefaultFlexibleFamilies(arm64) returned empty slice")
	}

	amd64Families := DefaultFlexibleFamilies("amd64")
	if len(amd64Families) == 0 {
		t.Error("DefaultFlexibleFamilies(amd64) returned empty slice")
	}

	// Verify they're different
	if len(arm64Families) == len(amd64Families) {
		allSame := true
		for i := range arm64Families {
			if arm64Families[i] != amd64Families[i] {
				allSame = false
				break
			}
		}
		if allSame {
			t.Error("DefaultFlexibleFamilies should return different families for arm64 and amd64")
		}
	}

	// Empty arch should return ARM64 families (legacy template uses ARM64 AMI)
	emptyArchFamilies := DefaultFlexibleFamilies("")
	if len(emptyArchFamilies) == 0 {
		t.Error("DefaultFlexibleFamilies(\"\") returned empty slice")
	}
	// Empty arch defaults to ARM64 families
	if len(emptyArchFamilies) != len(arm64Families) {
		t.Errorf("DefaultFlexibleFamilies(\"\") should return ARM64 families, got %d families, want %d", len(emptyArchFamilies), len(arm64Families))
	}
}
