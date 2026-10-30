package cache

import (
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
)

var refreshKeyPrefixes = []string{"setup-go-"}

func refreshEligible(key string) bool {
	for _, p := range refreshKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func markRefresh(key string) string {
	if strings.HasSuffix(key, wire.RefreshMarker) {
		return key
	}
	return key + wire.RefreshMarker
}

func unmarkRefresh(key string) string {
	if !refreshEligible(key) {
		return key
	}
	return strings.TrimSuffix(key, wire.RefreshMarker)
}
