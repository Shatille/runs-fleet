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
}

// InstanceCatalog contains specifications for supported EC2 instance types.
// This catalog is used for flexible instance type selection based on CPU/RAM requirements.
var InstanceCatalog = []InstanceSpec{
	// ARM64 (Graviton) - General Purpose
	{Type: "t4g.micro", CPU: 2, RAM: 1, Arch: "arm64", Family: "t4g"},
	{Type: "t4g.small", CPU: 2, RAM: 2, Arch: "arm64", Family: "t4g"},
	{Type: "t4g.medium", CPU: 2, RAM: 4, Arch: "arm64", Family: "t4g"},
	{Type: "t4g.large", CPU: 2, RAM: 8, Arch: "arm64", Family: "t4g"},
	{Type: "t4g.xlarge", CPU: 4, RAM: 16, Arch: "arm64", Family: "t4g"},
	{Type: "t4g.2xlarge", CPU: 8, RAM: 32, Arch: "arm64", Family: "t4g"},

	// ARM64 (Graviton3) - Compute Optimized
	{Type: "c7g.medium", CPU: 1, RAM: 2, Arch: "arm64", Family: "c7g"},
	{Type: "c7g.large", CPU: 2, RAM: 4, Arch: "arm64", Family: "c7g"},
	{Type: "c7g.xlarge", CPU: 4, RAM: 8, Arch: "arm64", Family: "c7g"},
	{Type: "c7g.2xlarge", CPU: 8, RAM: 16, Arch: "arm64", Family: "c7g"},
	{Type: "c7g.4xlarge", CPU: 16, RAM: 32, Arch: "arm64", Family: "c7g"},
	{Type: "c7g.8xlarge", CPU: 32, RAM: 64, Arch: "arm64", Family: "c7g"},
	{Type: "c7g.12xlarge", CPU: 48, RAM: 96, Arch: "arm64", Family: "c7g"},
	{Type: "c7g.16xlarge", CPU: 64, RAM: 128, Arch: "arm64", Family: "c7g"},

	// ARM64 (Graviton3) - Memory Optimized
	{Type: "m7g.medium", CPU: 1, RAM: 4, Arch: "arm64", Family: "m7g"},
	{Type: "m7g.large", CPU: 2, RAM: 8, Arch: "arm64", Family: "m7g"},
	{Type: "m7g.xlarge", CPU: 4, RAM: 16, Arch: "arm64", Family: "m7g"},
	{Type: "m7g.2xlarge", CPU: 8, RAM: 32, Arch: "arm64", Family: "m7g"},
	{Type: "m7g.4xlarge", CPU: 16, RAM: 64, Arch: "arm64", Family: "m7g"},
	{Type: "m7g.8xlarge", CPU: 32, RAM: 128, Arch: "arm64", Family: "m7g"},
	{Type: "m7g.12xlarge", CPU: 48, RAM: 192, Arch: "arm64", Family: "m7g"},
	{Type: "m7g.16xlarge", CPU: 64, RAM: 256, Arch: "arm64", Family: "m7g"},

	// ARM64 (Graviton3) - High Memory
	{Type: "r7g.medium", CPU: 1, RAM: 8, Arch: "arm64", Family: "r7g"},
	{Type: "r7g.large", CPU: 2, RAM: 16, Arch: "arm64", Family: "r7g"},
	{Type: "r7g.xlarge", CPU: 4, RAM: 32, Arch: "arm64", Family: "r7g"},
	{Type: "r7g.2xlarge", CPU: 8, RAM: 64, Arch: "arm64", Family: "r7g"},
	{Type: "r7g.4xlarge", CPU: 16, RAM: 128, Arch: "arm64", Family: "r7g"},
	{Type: "r7g.8xlarge", CPU: 32, RAM: 256, Arch: "arm64", Family: "r7g"},
	{Type: "r7g.12xlarge", CPU: 48, RAM: 384, Arch: "arm64", Family: "r7g"},
	{Type: "r7g.16xlarge", CPU: 64, RAM: 512, Arch: "arm64", Family: "r7g"},

	// amd64 - Burstable
	{Type: "t3.micro", CPU: 2, RAM: 1, Arch: "amd64", Family: "t3"},
	{Type: "t3.small", CPU: 2, RAM: 2, Arch: "amd64", Family: "t3"},
	{Type: "t3.medium", CPU: 2, RAM: 4, Arch: "amd64", Family: "t3"},
	{Type: "t3.large", CPU: 2, RAM: 8, Arch: "amd64", Family: "t3"},
	{Type: "t3.xlarge", CPU: 4, RAM: 16, Arch: "amd64", Family: "t3"},
	{Type: "t3.2xlarge", CPU: 8, RAM: 32, Arch: "amd64", Family: "t3"},

	// amd64 - Compute Optimized (6th gen)
	{Type: "c6i.large", CPU: 2, RAM: 4, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.xlarge", CPU: 4, RAM: 8, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.2xlarge", CPU: 8, RAM: 16, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.4xlarge", CPU: 16, RAM: 32, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.8xlarge", CPU: 32, RAM: 64, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.12xlarge", CPU: 48, RAM: 96, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.16xlarge", CPU: 64, RAM: 128, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.24xlarge", CPU: 96, RAM: 192, Arch: "amd64", Family: "c6i"},
	{Type: "c6i.32xlarge", CPU: 128, RAM: 256, Arch: "amd64", Family: "c6i"},

	// amd64 - Compute Optimized (7th gen)
	{Type: "c7i.large", CPU: 2, RAM: 4, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.xlarge", CPU: 4, RAM: 8, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.2xlarge", CPU: 8, RAM: 16, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.4xlarge", CPU: 16, RAM: 32, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.8xlarge", CPU: 32, RAM: 64, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.12xlarge", CPU: 48, RAM: 96, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.16xlarge", CPU: 64, RAM: 128, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.24xlarge", CPU: 96, RAM: 192, Arch: "amd64", Family: "c7i"},
	{Type: "c7i.48xlarge", CPU: 192, RAM: 384, Arch: "amd64", Family: "c7i"},

	// amd64 - Memory Optimized (6th gen)
	{Type: "m6i.large", CPU: 2, RAM: 8, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.xlarge", CPU: 4, RAM: 16, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.2xlarge", CPU: 8, RAM: 32, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.4xlarge", CPU: 16, RAM: 64, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.8xlarge", CPU: 32, RAM: 128, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.12xlarge", CPU: 48, RAM: 192, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.16xlarge", CPU: 64, RAM: 256, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.24xlarge", CPU: 96, RAM: 384, Arch: "amd64", Family: "m6i"},
	{Type: "m6i.32xlarge", CPU: 128, RAM: 512, Arch: "amd64", Family: "m6i"},

	// amd64 - Memory Optimized (7th gen)
	{Type: "m7i.large", CPU: 2, RAM: 8, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.xlarge", CPU: 4, RAM: 16, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.2xlarge", CPU: 8, RAM: 32, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.4xlarge", CPU: 16, RAM: 64, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.8xlarge", CPU: 32, RAM: 128, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.12xlarge", CPU: 48, RAM: 192, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.16xlarge", CPU: 64, RAM: 256, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.24xlarge", CPU: 96, RAM: 384, Arch: "amd64", Family: "m7i"},
	{Type: "m7i.48xlarge", CPU: 192, RAM: 768, Arch: "amd64", Family: "m7i"},

	// amd64 - High Memory (6th gen)
	{Type: "r6i.large", CPU: 2, RAM: 16, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.xlarge", CPU: 4, RAM: 32, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.2xlarge", CPU: 8, RAM: 64, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.4xlarge", CPU: 16, RAM: 128, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.8xlarge", CPU: 32, RAM: 256, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.12xlarge", CPU: 48, RAM: 384, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.16xlarge", CPU: 64, RAM: 512, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.24xlarge", CPU: 96, RAM: 768, Arch: "amd64", Family: "r6i"},
	{Type: "r6i.32xlarge", CPU: 128, RAM: 1024, Arch: "amd64", Family: "r6i"},

	// amd64 - High Memory (7th gen)
	{Type: "r7i.large", CPU: 2, RAM: 16, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.xlarge", CPU: 4, RAM: 32, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.2xlarge", CPU: 8, RAM: 64, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.4xlarge", CPU: 16, RAM: 128, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.8xlarge", CPU: 32, RAM: 256, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.12xlarge", CPU: 48, RAM: 384, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.16xlarge", CPU: 64, RAM: 512, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.24xlarge", CPU: 96, RAM: 768, Arch: "amd64", Family: "r7i"},
	{Type: "r7i.48xlarge", CPU: 192, RAM: 1536, Arch: "amd64", Family: "r7i"},
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
// Empty arch defaults to ARM64 families (legacy template uses ARM64 AMI).
func DefaultFlexibleFamilies(arch string) []string {
	if arch == "amd64" {
		return []string{"c6i", "c7i", "m6i", "m7i", "t3"}
	}
	// ARM64 or empty arch - empty defaults to ARM64 since legacy template uses ARM64 AMI
	return []string{"c7g", "m7g", "t4g"}
}
