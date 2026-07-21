package cost

import (
	"strings"
	"testing"
)

func TestSpotInterruptionQueries(t *testing.T) {
	t.Parallel()
	queries := spotInterruptionQueries()
	if len(queries) != 1 {
		t.Fatalf("got %d queries, want 1", len(queries))
	}
	q := queries[0]
	if q.Id == nil || *q.Id != spotInterruptionQueryID {
		t.Fatalf("query ID = %v, want %q", q.Id, spotInterruptionQueryID)
	}
	if q.Expression == nil {
		t.Fatal("query missing Expression")
	}
	expr := *q.Expression
	if !strings.HasPrefix(expr, "SUM(SEARCH(") {
		t.Errorf("expr should sum a search: %s", expr)
	}
	if !strings.Contains(expr, `MetricName="SpotInterruptions"`) {
		t.Errorf("expr missing metric name: %s", expr)
	}
	if !strings.Contains(expr, "{RunsFleet,Family}") {
		t.Errorf("expr should search the Family dimension: %s", expr)
	}
}
