package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

const testPathRunnersMyOrg = "/repos/myorg/myrepo/actions/runners"

func TestClient_ListRunners_PaginatesAndMapsFields(t *testing.T) {
	var seenPages []string

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnersMyOrg {
			return false
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		page := r.URL.Query().Get("page")
		seenPages = append(seenPages, page)
		w.WriteHeader(http.StatusOK)
		if page == "2" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 101,
				"runners": []map[string]any{
					{"id": 2, "name": "runs-fleet-runner-b", "status": "online", "busy": true},
				},
			})
			return true
		}
		runners := make([]map[string]any, 0, 100)
		runners = append(runners, map[string]any{
			"id": 1, "name": "runs-fleet-runner-a", "status": "offline", "busy": false,
		})
		for i := 2; i <= 100; i++ {
			runners = append(runners, map[string]any{
				"id": 1000 + i, "name": fmt.Sprintf("filler-%d", i), "status": "offline", "busy": false,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "runners": runners})
		return true
	}))

	got, err := client.ListRunners(context.Background(), "myorg/myrepo")
	if err != nil {
		t.Fatalf("ListRunners() error = %v", err)
	}
	if len(got) != 101 {
		t.Fatalf("got %d runners, want 101 (pagination truncated)", len(got))
	}
	if len(seenPages) < 2 {
		t.Errorf("expected at least 2 pages fetched, saw %v", seenPages)
	}
	if got[0].ID != 1 || got[0].Name != "runs-fleet-runner-a" || got[0].Status != "offline" || got[0].Busy {
		t.Errorf("first runner mapped wrong: %+v", got[0])
	}
	last := got[len(got)-1]
	if last.ID != 2 || !last.Busy {
		t.Errorf("last runner mapped wrong: %+v", last)
	}
}

func TestClient_DeleteRunner_SendsDeleteAndAcceptsNoContent(t *testing.T) {
	var gotMethod, gotPath string

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnersMyOrg+"/42" {
			return false
		}
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
		return true
	}))

	if err := client.DeleteRunner(context.Background(), "myorg/myrepo", 42); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != testPathRunnersMyOrg+"/42" {
		t.Errorf("path = %s, want %s/42", gotPath, testPathRunnersMyOrg)
	}
}

// A runner that took a job between the sweep's list and its delete is removed by
// GitHub the moment that job ends, so the delete races into a 404. That is the
// sweep's own goal reached by another route, not a failure to retry.
func TestClient_DeleteRunner_TreatsNotFoundAsSuccess(t *testing.T) {
	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnersMyOrg+"/7" {
			return false
		}
		w.WriteHeader(http.StatusNotFound)
		return true
	}))

	if err := client.DeleteRunner(context.Background(), "myorg/myrepo", 7); err != nil {
		t.Errorf("DeleteRunner() on 404 error = %v, want nil", err)
	}
}

// A busy runner returns 422; deleting it would kill a running job, so the error
// must surface rather than be retried into a loop.
func TestClient_DeleteRunner_SurfacesConflict(t *testing.T) {
	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnersMyOrg+"/9" {
			return false
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		return true
	}))

	if err := client.DeleteRunner(context.Background(), "myorg/myrepo", 9); err == nil {
		t.Error("DeleteRunner() on 422 error = nil, want an error")
	}
}
