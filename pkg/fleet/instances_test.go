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

	// Empty arch should return both ARM64 and AMD64 families for diversification
	emptyArchFamilies := DefaultFlexibleFamilies("")
	if len(emptyArchFamilies) == 0 {
		t.Error("DefaultFlexibleFamilies(\"\") returned empty slice")
	}
	// Empty arch returns combined families (ARM64 + AMD64)
	expectedLen := len(arm64Families) + len(amd64Families)
	if len(emptyArchFamilies) != expectedLen {
		t.Errorf("DefaultFlexibleFamilies(\"\") should return combined families, got %d families, want %d", len(emptyArchFamilies), expectedLen)
	}
	// Verify ARM64 families are included
	for _, f := range arm64Families {
		found := false
		for _, ef := range emptyArchFamilies {
			if ef == f {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultFlexibleFamilies(\"\") missing ARM64 family %q", f)
		}
	}
	// Verify AMD64 families are included
	for _, f := range amd64Families {
		found := false
		for _, ef := range emptyArchFamilies {
			if ef == f {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultFlexibleFamilies(\"\") missing AMD64 family %q", f)
		}
	}
}

func TestResolveInstanceTypes_Generation(t *testing.T) {
	tests := []struct {
		name     string
		spec     FlexibleSpec
		contains []string
		excludes []string
	}{
		{
			name: "Gen 8 ARM64 only",
			spec: FlexibleSpec{
				CPUMin: 4,
				CPUMax: 4,
				Arch:   "arm64",
				Gen:    8,
			},
			contains: []string{"c8g.xlarge", "m8g.xlarge"},
			excludes: []string{"c7g.xlarge", "m7g.xlarge", "t4g.xlarge"},
		},
		{
			name: "Gen 7 ARM64 only",
			spec: FlexibleSpec{
				CPUMin: 4,
				CPUMax: 4,
				Arch:   "arm64",
				Gen:    7,
			},
			contains: []string{"c7g.xlarge", "m7g.xlarge", "r7g.xlarge"},
			excludes: []string{"c8g.xlarge", "m8g.xlarge", "t4g.xlarge"},
		},
		{
			name: "Gen 7 amd64 only",
			spec: FlexibleSpec{
				CPUMin: 4,
				CPUMax: 4,
				Arch:   "amd64",
				Gen:    7,
			},
			contains: []string{"c7i.xlarge", "m7i.xlarge", "r7i.xlarge"},
			excludes: []string{"c6i.xlarge", "m6i.xlarge", "t3.xlarge"},
		},
		{
			name: "Gen 6 amd64 only",
			spec: FlexibleSpec{
				CPUMin: 4,
				CPUMax: 4,
				Arch:   "amd64",
				Gen:    6,
			},
			contains: []string{"c6i.xlarge", "m6i.xlarge", "r6i.xlarge"},
			excludes: []string{"c7i.xlarge", "m7i.xlarge", "t3.xlarge"},
		},
		{
			name: "Gen 0 (any) returns all generations",
			spec: FlexibleSpec{
				CPUMin:   4,
				CPUMax:   4,
				Arch:     "arm64",
				Families: []string{"c7g", "c8g"},
				Gen:      0,
			},
			contains: []string{"c7g.xlarge", "c8g.xlarge"},
		},
		{
			name: "Gen with family filter",
			spec: FlexibleSpec{
				CPUMin:   4,
				CPUMax:   8,
				Arch:     "arm64",
				Families: []string{"c8g", "m8g"},
				Gen:      8,
			},
			contains: []string{"c8g.xlarge", "c8g.2xlarge", "m8g.xlarge", "m8g.2xlarge"},
			excludes: []string{"c7g.xlarge", "r8g.xlarge"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ResolveInstanceTypes(tt.spec)

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

func TestGetInstanceArch(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		wantArch     string
	}{
		{
			name:         "ARM64 c7g instance",
			instanceType: "c7g.xlarge",
			wantArch:     "arm64",
		},
		{
			name:         "ARM64 t4g instance",
			instanceType: "t4g.medium",
			wantArch:     "arm64",
		},
		{
			name:         "AMD64 c6i instance",
			instanceType: "c6i.xlarge",
			wantArch:     "amd64",
		},
		{
			name:         "AMD64 t3 instance",
			instanceType: "t3.medium",
			wantArch:     "amd64",
		},
		{
			name:         "Unknown instance type",
			instanceType: "unknown.type",
			wantArch:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetInstanceArch(tt.instanceType)
			if got != tt.wantArch {
				t.Errorf("GetInstanceArch(%q) = %q, want %q", tt.instanceType, got, tt.wantArch)
			}
		})
	}
}

func TestGroupInstanceTypesByArch(t *testing.T) {
	tests := []struct {
		name          string
		instanceTypes []string
		wantArm64     []string
		wantAmd64     []string
	}{
		{
			name:          "Mixed architectures",
			instanceTypes: []string{"c7g.xlarge", "t3.medium", "t4g.medium", "c6i.large"},
			wantArm64:     []string{"c7g.xlarge", "t4g.medium"},
			wantAmd64:     []string{"t3.medium", "c6i.large"},
		},
		{
			name:          "ARM64 only",
			instanceTypes: []string{"c7g.xlarge", "t4g.medium", "m7g.large"},
			wantArm64:     []string{"c7g.xlarge", "t4g.medium", "m7g.large"},
			wantAmd64:     nil,
		},
		{
			name:          "AMD64 only",
			instanceTypes: []string{"t3.medium", "c6i.large", "m6i.xlarge"},
			wantArm64:     nil,
			wantAmd64:     []string{"t3.medium", "c6i.large", "m6i.xlarge"},
		},
		{
			name:          "Unknown types filtered out",
			instanceTypes: []string{"c7g.xlarge", "unknown.type", "t3.medium"},
			wantArm64:     []string{"c7g.xlarge"},
			wantAmd64:     []string{"t3.medium"},
		},
		{
			name:          "All unknown types",
			instanceTypes: []string{"unknown.type", "fake.instance"},
			wantArm64:     nil,
			wantAmd64:     nil,
		},
		{
			name:          "Empty input",
			instanceTypes: []string{},
			wantArm64:     nil,
			wantAmd64:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GroupInstanceTypesByArch(tt.instanceTypes)

			// Check ARM64
			arm64Types := result["arm64"]
			if len(arm64Types) != len(tt.wantArm64) {
				t.Errorf("arm64 types: got %v, want %v", arm64Types, tt.wantArm64)
			} else {
				for i, want := range tt.wantArm64 {
					if arm64Types[i] != want {
						t.Errorf("arm64[%d]: got %q, want %q", i, arm64Types[i], want)
					}
				}
			}

			// Check AMD64
			amd64Types := result["amd64"]
			if len(amd64Types) != len(tt.wantAmd64) {
				t.Errorf("amd64 types: got %v, want %v", amd64Types, tt.wantAmd64)
			} else {
				for i, want := range tt.wantAmd64 {
					if amd64Types[i] != want {
						t.Errorf("amd64[%d]: got %q, want %q", i, amd64Types[i], want)
					}
				}
			}
		})
	}
}
