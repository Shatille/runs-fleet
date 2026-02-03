// Package fleet manages EC2 fleet creation and lifecycle for runner instances.
package fleet

import (
	"sort"
	"strings"
)

// InstanceSpec defines the specifications of an EC2 instance type.
type InstanceSpec struct {
	Type   string  // EC2 instance type (e.g., "c7g.xlarge")
	CPU    int     // Number of vCPUs
	RAM    float64 // Memory in GB
	Arch   string  // Architecture: "amd64" or "arm64"
	Family string  // Instance family (e.g., "c7g", "m7g", "t4g")
	Gen    int     // Instance generation (e.g., 7 for c7g, 8 for c8g)
}

// InstanceCatalog contains specifications for supported EC2 instance types.
// This catalog is used for flexible instance type selection based on CPU/RAM requirements.
var InstanceCatalog = []InstanceSpec{
	// ARM64 (Graviton2) - General Purpose (4th gen)
	{Type: "t4g.micro", CPU: 2, RAM: 1, Arch: "arm64", Family: "t4g", Gen: 4},
	{Type: "t4g.small", CPU: 2, RAM: 2, Arch: "arm64", Family: "t4g", Gen: 4},
	{Type: "t4g.medium", CPU: 2, RAM: 4, Arch: "arm64", Family: "t4g", Gen: 4},
	{Type: "t4g.large", CPU: 2, RAM: 8, Arch: "arm64", Family: "t4g", Gen: 4},
	{Type: "t4g.xlarge", CPU: 4, RAM: 16, Arch: "arm64", Family: "t4g", Gen: 4},
	{Type: "t4g.2xlarge", CPU: 8, RAM: 32, Arch: "arm64", Family: "t4g", Gen: 4},

	// ARM64 (Graviton3) - Compute Optimized (7th gen)
	{Type: "c7g.medium", CPU: 1, RAM: 2, Arch: "arm64", Family: "c7g", Gen: 7},
	{Type: "c7g.large", CPU: 2, RAM: 4, Arch: "arm64", Family: "c7g", Gen: 7},
	{Type: "c7g.xlarge", CPU: 4, RAM: 8, Arch: "arm64", Family: "c7g", Gen: 7},
	{Type: "c7g.2xlarge", CPU: 8, RAM: 16, Arch: "arm64", Family: "c7g", Gen: 7},
	{Type: "c7g.4xlarge", CPU: 16, RAM: 32, Arch: "arm64", Family: "c7g", Gen: 7},
	{Type: "c7g.8xlarge", CPU: 32, RAM: 64, Arch: "arm64", Family: "c7g", Gen: 7},
	{Type: "c7g.12xlarge", CPU: 48, RAM: 96, Arch: "arm64", Family: "c7g", Gen: 7},
	{Type: "c7g.16xlarge", CPU: 64, RAM: 128, Arch: "arm64", Family: "c7g", Gen: 7},

	// ARM64 (Graviton3) - Memory Optimized (7th gen)
	{Type: "m7g.medium", CPU: 1, RAM: 4, Arch: "arm64", Family: "m7g", Gen: 7},
	{Type: "m7g.large", CPU: 2, RAM: 8, Arch: "arm64", Family: "m7g", Gen: 7},
	{Type: "m7g.xlarge", CPU: 4, RAM: 16, Arch: "arm64", Family: "m7g", Gen: 7},
	{Type: "m7g.2xlarge", CPU: 8, RAM: 32, Arch: "arm64", Family: "m7g", Gen: 7},
	{Type: "m7g.4xlarge", CPU: 16, RAM: 64, Arch: "arm64", Family: "m7g", Gen: 7},
	{Type: "m7g.8xlarge", CPU: 32, RAM: 128, Arch: "arm64", Family: "m7g", Gen: 7},
	{Type: "m7g.12xlarge", CPU: 48, RAM: 192, Arch: "arm64", Family: "m7g", Gen: 7},
	{Type: "m7g.16xlarge", CPU: 64, RAM: 256, Arch: "arm64", Family: "m7g", Gen: 7},

	// ARM64 (Graviton3) - High Memory (7th gen)
	{Type: "r7g.medium", CPU: 1, RAM: 8, Arch: "arm64", Family: "r7g", Gen: 7},
	{Type: "r7g.large", CPU: 2, RAM: 16, Arch: "arm64", Family: "r7g", Gen: 7},
	{Type: "r7g.xlarge", CPU: 4, RAM: 32, Arch: "arm64", Family: "r7g", Gen: 7},
	{Type: "r7g.2xlarge", CPU: 8, RAM: 64, Arch: "arm64", Family: "r7g", Gen: 7},
	{Type: "r7g.4xlarge", CPU: 16, RAM: 128, Arch: "arm64", Family: "r7g", Gen: 7},
	{Type: "r7g.8xlarge", CPU: 32, RAM: 256, Arch: "arm64", Family: "r7g", Gen: 7},
	{Type: "r7g.12xlarge", CPU: 48, RAM: 384, Arch: "arm64", Family: "r7g", Gen: 7},
	{Type: "r7g.16xlarge", CPU: 64, RAM: 512, Arch: "arm64", Family: "r7g", Gen: 7},

	// ARM64 (Graviton4) - Compute Optimized (8th gen)
	{Type: "c8g.medium", CPU: 1, RAM: 2, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.large", CPU: 2, RAM: 4, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.xlarge", CPU: 4, RAM: 8, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.2xlarge", CPU: 8, RAM: 16, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.4xlarge", CPU: 16, RAM: 32, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.8xlarge", CPU: 32, RAM: 64, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.12xlarge", CPU: 48, RAM: 96, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.16xlarge", CPU: 64, RAM: 128, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.24xlarge", CPU: 96, RAM: 192, Arch: "arm64", Family: "c8g", Gen: 8},
	{Type: "c8g.48xlarge", CPU: 192, RAM: 384, Arch: "arm64", Family: "c8g", Gen: 8},

	// ARM64 (Graviton4) - Memory Optimized (8th gen)
	{Type: "m8g.medium", CPU: 1, RAM: 4, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.large", CPU: 2, RAM: 8, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.xlarge", CPU: 4, RAM: 16, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.2xlarge", CPU: 8, RAM: 32, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.4xlarge", CPU: 16, RAM: 64, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.8xlarge", CPU: 32, RAM: 128, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.12xlarge", CPU: 48, RAM: 192, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.16xlarge", CPU: 64, RAM: 256, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.24xlarge", CPU: 96, RAM: 384, Arch: "arm64", Family: "m8g", Gen: 8},
	{Type: "m8g.48xlarge", CPU: 192, RAM: 768, Arch: "arm64", Family: "m8g", Gen: 8},

	// ARM64 (Graviton4) - High Memory (8th gen)
	{Type: "r8g.medium", CPU: 1, RAM: 8, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.large", CPU: 2, RAM: 16, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.xlarge", CPU: 4, RAM: 32, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.2xlarge", CPU: 8, RAM: 64, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.4xlarge", CPU: 16, RAM: 128, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.8xlarge", CPU: 32, RAM: 256, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.12xlarge", CPU: 48, RAM: 384, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.16xlarge", CPU: 64, RAM: 512, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.24xlarge", CPU: 96, RAM: 768, Arch: "arm64", Family: "r8g", Gen: 8},
	{Type: "r8g.48xlarge", CPU: 192, RAM: 1536, Arch: "arm64", Family: "r8g", Gen: 8},

	// amd64 - Burstable (3rd gen)
	{Type: "t3.micro", CPU: 2, RAM: 1, Arch: "amd64", Family: "t3", Gen: 3},
	{Type: "t3.small", CPU: 2, RAM: 2, Arch: "amd64", Family: "t3", Gen: 3},
	{Type: "t3.medium", CPU: 2, RAM: 4, Arch: "amd64", Family: "t3", Gen: 3},
	{Type: "t3.large", CPU: 2, RAM: 8, Arch: "amd64", Family: "t3", Gen: 3},
	{Type: "t3.xlarge", CPU: 4, RAM: 16, Arch: "amd64", Family: "t3", Gen: 3},
	{Type: "t3.2xlarge", CPU: 8, RAM: 32, Arch: "amd64", Family: "t3", Gen: 3},

	// amd64 - Compute Optimized (6th gen)
	{Type: "c6i.large", CPU: 2, RAM: 4, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.xlarge", CPU: 4, RAM: 8, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.2xlarge", CPU: 8, RAM: 16, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.4xlarge", CPU: 16, RAM: 32, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.8xlarge", CPU: 32, RAM: 64, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.12xlarge", CPU: 48, RAM: 96, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.16xlarge", CPU: 64, RAM: 128, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.24xlarge", CPU: 96, RAM: 192, Arch: "amd64", Family: "c6i", Gen: 6},
	{Type: "c6i.32xlarge", CPU: 128, RAM: 256, Arch: "amd64", Family: "c6i", Gen: 6},

	// amd64 - Compute Optimized (7th gen)
	{Type: "c7i.large", CPU: 2, RAM: 4, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.xlarge", CPU: 4, RAM: 8, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.2xlarge", CPU: 8, RAM: 16, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.4xlarge", CPU: 16, RAM: 32, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.8xlarge", CPU: 32, RAM: 64, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.12xlarge", CPU: 48, RAM: 96, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.16xlarge", CPU: 64, RAM: 128, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.24xlarge", CPU: 96, RAM: 192, Arch: "amd64", Family: "c7i", Gen: 7},
	{Type: "c7i.48xlarge", CPU: 192, RAM: 384, Arch: "amd64", Family: "c7i", Gen: 7},

	// amd64 - Memory Optimized (6th gen)
	{Type: "m6i.large", CPU: 2, RAM: 8, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.xlarge", CPU: 4, RAM: 16, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.2xlarge", CPU: 8, RAM: 32, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.4xlarge", CPU: 16, RAM: 64, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.8xlarge", CPU: 32, RAM: 128, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.12xlarge", CPU: 48, RAM: 192, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.16xlarge", CPU: 64, RAM: 256, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.24xlarge", CPU: 96, RAM: 384, Arch: "amd64", Family: "m6i", Gen: 6},
	{Type: "m6i.32xlarge", CPU: 128, RAM: 512, Arch: "amd64", Family: "m6i", Gen: 6},

	// amd64 - Memory Optimized (7th gen)
	{Type: "m7i.large", CPU: 2, RAM: 8, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.xlarge", CPU: 4, RAM: 16, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.2xlarge", CPU: 8, RAM: 32, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.4xlarge", CPU: 16, RAM: 64, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.8xlarge", CPU: 32, RAM: 128, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.12xlarge", CPU: 48, RAM: 192, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.16xlarge", CPU: 64, RAM: 256, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.24xlarge", CPU: 96, RAM: 384, Arch: "amd64", Family: "m7i", Gen: 7},
	{Type: "m7i.48xlarge", CPU: 192, RAM: 768, Arch: "amd64", Family: "m7i", Gen: 7},

	// amd64 - High Memory (6th gen)
	{Type: "r6i.large", CPU: 2, RAM: 16, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.xlarge", CPU: 4, RAM: 32, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.2xlarge", CPU: 8, RAM: 64, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.4xlarge", CPU: 16, RAM: 128, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.8xlarge", CPU: 32, RAM: 256, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.12xlarge", CPU: 48, RAM: 384, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.16xlarge", CPU: 64, RAM: 512, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.24xlarge", CPU: 96, RAM: 768, Arch: "amd64", Family: "r6i", Gen: 6},
	{Type: "r6i.32xlarge", CPU: 128, RAM: 1024, Arch: "amd64", Family: "r6i", Gen: 6},

	// amd64 - High Memory (7th gen)
	{Type: "r7i.large", CPU: 2, RAM: 16, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.xlarge", CPU: 4, RAM: 32, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.2xlarge", CPU: 8, RAM: 64, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.4xlarge", CPU: 16, RAM: 128, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.8xlarge", CPU: 32, RAM: 256, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.12xlarge", CPU: 48, RAM: 384, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.16xlarge", CPU: 64, RAM: 512, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.24xlarge", CPU: 96, RAM: 768, Arch: "amd64", Family: "r7i", Gen: 7},
	{Type: "r7i.48xlarge", CPU: 192, RAM: 1536, Arch: "amd64", Family: "r7i", Gen: 7},
}

// instanceCatalogByType provides O(1) lookup by instance type.
var instanceCatalogByType map[string]InstanceSpec

func init() {
	instanceCatalogByType = make(map[string]InstanceSpec, len(InstanceCatalog))
	for _, spec := range InstanceCatalog {
		instanceCatalogByType[spec.Type] = spec
	}
}

// GetInstanceSpec returns the specification for a given instance type.
func GetInstanceSpec(instanceType string) (InstanceSpec, bool) {
	spec, ok := instanceCatalogByType[instanceType]
	return spec, ok
}

// FlexibleSpec defines flexible resource requirements for instance selection.
type FlexibleSpec struct {
	CPUMin   int      // Minimum vCPUs required
	CPUMax   int      // Maximum vCPUs acceptable (0 = no max)
	RAMMin   float64  // Minimum RAM in GB
	RAMMax   float64  // Maximum RAM in GB (0 = no max)
	Arch     string   // Required architecture: "amd64" or "arm64"
	Families []string // Preferred instance families (empty = all families)
	Gen      int      // Required instance generation (0 = any generation)
}

// ResolveInstanceTypes finds all instance types matching the flexible spec.
// Returns instance types sorted by CPU (ascending) for optimal spot availability.
func ResolveInstanceTypes(spec FlexibleSpec) []string {
	var matches []InstanceSpec

	for _, inst := range InstanceCatalog {
		// Check architecture
		if spec.Arch != "" && inst.Arch != spec.Arch {
			continue
		}

		// Check generation
		if spec.Gen > 0 && inst.Gen != spec.Gen {
			continue
		}

		// Check CPU range
		if inst.CPU < spec.CPUMin {
			continue
		}
		if spec.CPUMax > 0 && inst.CPU > spec.CPUMax {
			continue
		}

		// Check RAM range
		if inst.RAM < spec.RAMMin {
			continue
		}
		if spec.RAMMax > 0 && inst.RAM > spec.RAMMax {
			continue
		}

		// Check family filter
		if len(spec.Families) > 0 {
			familyMatch := false
			for _, f := range spec.Families {
				if strings.EqualFold(inst.Family, f) {
					familyMatch = true
					break
				}
			}
			if !familyMatch {
				continue
			}
		}

		matches = append(matches, inst)
	}

	// Sort by CPU count (ascending) for better spot availability
	// Smaller instances are generally more available
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].CPU != matches[j].CPU {
			return matches[i].CPU < matches[j].CPU
		}
		return matches[i].RAM < matches[j].RAM
	})

	result := make([]string, len(matches))
	for i, m := range matches {
		result[i] = m.Type
	}

	return result
}

// DefaultFlexibleFamilies returns the default instance families for an architecture.
// When arch is empty, returns both ARM64 and AMD64 families for maximum spot diversification.
// Includes 8th gen families (c8g, m8g, r8g) which may not be available in all regions.
func DefaultFlexibleFamilies(arch string) []string {
	switch arch {
	case "amd64":
		return []string{"c6i", "c7i", "m6i", "m7i", "t3"}
	case "arm64":
		return []string{"c8g", "m8g", "r8g", "c7g", "m7g", "t4g"}
	default:
		// No arch preference - include both for diversification
		return []string{"c8g", "m8g", "r8g", "c7g", "m7g", "t4g", "c6i", "c7i", "m6i", "m7i", "t3"}
	}
}

// GetInstanceArch returns the architecture for a given instance type.
// Returns empty string if the instance type is not in the catalog.
func GetInstanceArch(instanceType string) string {
	if spec, ok := instanceCatalogByType[instanceType]; ok {
		return spec.Arch
	}
	return ""
}

// GroupInstanceTypesByArch groups instance types by their architecture.
// Returns a map of arch -> []instanceType. Unknown instance types are skipped.
func GroupInstanceTypesByArch(instanceTypes []string) map[string][]string {
	result := make(map[string][]string)
	for _, instType := range instanceTypes {
		arch := GetInstanceArch(instType)
		if arch != "" {
			result[arch] = append(result[arch], instType)
		}
	}
	return result
}
