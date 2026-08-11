package admin

import (
	"net/http"
	"slices"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/fleet"
)

// CurrentAMI is what a new instance of one architecture would boot today.
type CurrentAMI struct {
	Architecture   string `json:"architecture"`
	ImageID        string `json:"image_id"`
	LaunchTemplate string `json:"launch_template"`
	Version        int64  `json:"version"`
	VersionCreated string `json:"version_created,omitempty"`
}

// CurrentAMIsResponse reports the reference AMI per architecture.
type CurrentAMIsResponse struct {
	AMIs []CurrentAMI `json:"amis"`
	// Unresolved names architectures whose template could not be read. Their
	// instances are reported with no staleness rather than a guessed one.
	Unresolved []string `json:"unresolved,omitempty"`
}

// SetAMISource wires the launch-template reader that makes AMI staleness
// answerable. Without it the instances list still renders, reporting that it
// does not know which AMI is current rather than guessing.
func (h *InstancesHandler) SetAMISource(api fleet.LaunchTemplateAPI, launchTemplateName string) {
	h.amis = fleet.NewAMIResolver(api, launchTemplateName)
}

// SetAMIResolver shares an already-built resolver, so the console and the
// housekeeping sweep read one cache and cannot disagree about what is current.
func (h *InstancesHandler) SetAMIResolver(r *fleet.AMIResolver) {
	h.amis = r
}

// CurrentAMIs handles GET /api/instances/amis.
func (h *InstancesHandler) CurrentAMIs(w http.ResponseWriter, r *http.Request) {
	if h.amis == nil {
		h.writeError(w, http.StatusServiceUnavailable, "AMI source not configured",
			"no launch-template reader is wired into the orchestrator")
		return
	}

	current, err := h.amis.Current(r.Context())
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "Failed to read launch templates", err.Error())
		return
	}

	resp := CurrentAMIsResponse{Unresolved: h.amis.UnresolvedArchs()}
	for _, arch := range sortedArchs(current) {
		ami := current[arch]
		out := CurrentAMI{
			Architecture:   ami.Architecture,
			ImageID:        ami.ImageID,
			LaunchTemplate: ami.LaunchTemplate,
			Version:        ami.Version,
		}
		if !ami.VersionCreated.IsZero() {
			out.VersionCreated = ami.VersionCreated.Format(time.RFC3339)
		}
		resp.AMIs = append(resp.AMIs, out)
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// sortedArchs keeps the response order stable across requests; map iteration
// would otherwise reshuffle the card on every refresh.
func sortedArchs(m map[string]fleet.CurrentAMI) []string {
	archs := make([]string, 0, len(m))
	for a := range m {
		archs = append(archs, a)
	}
	slices.Sort(archs)
	return archs
}
