package gitops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPushEvent_GetModifiedFiles(t *testing.T) {
	tests := []struct {
		name     string
		event    PushEvent
		expected int
	}{
		{
			name: "single commit with all types",
			event: PushEvent{
				Commits: []Commit{
					{
						Added:    []string{"file1.go"},
						Modified: []string{"file2.go"},
						Removed:  []string{"file3.go"},
					},
				},
			},
			expected: 3,
		},
		{
			name: "multiple commits without duplicates",
			event: PushEvent{
				Commits: []Commit{
					{
						Added: []string{"file1.go"},
					},
					{
						Modified: []string{"file2.go"},
					},
				},
			},
			expected: 2,
		},
		{
			name: "multiple commits with duplicates",
			event: PushEvent{
				Commits: []Commit{
					{
						Added: []string{"file1.go"},
					},
					{
						Modified: []string{"file1.go"}, // Duplicate
					},
				},
			},
			expected: 1,
		},
		{
			name: "no commits",
			event: PushEvent{
				Commits: []Commit{},
			},
			expected: 0,
		},
		{
			name: "empty file lists",
			event: PushEvent{
				Commits: []Commit{
					{
						Added:    []string{},
						Modified: []string{},
						Removed:  []string{},
					},
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := tt.event.GetModifiedFiles()
			if len(files) != tt.expected {
				t.Errorf("GetModifiedFiles() returned %d files, expected %d", len(files), tt.expected)
			}
		})
	}
}

func TestPushEvent_GetBranch(t *testing.T) {
	tests := []struct {
		name     string
		ref      string
		expected string
	}{
		{
			name:     "main branch",
			ref:      "refs/heads/main",
			expected: "main",
		},
		{
			name:     "feature branch",
			ref:      "refs/heads/feature/new-feature",
			expected: "feature/new-feature",
		},
		{
			name:     "develop branch",
			ref:      "refs/heads/develop",
			expected: "develop",
		},
		{
			name:     "tag ref",
			ref:      "refs/tags/v1.0.0",
			expected: "refs/tags/v1.0.0",
		},
		{
			name:     "short ref",
			ref:      "main",
			expected: "main",
		},
		{
			name:     "empty ref",
			ref:      "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := PushEvent{Ref: tt.ref}
			branch := event.GetBranch()
			if branch != tt.expected {
				t.Errorf("GetBranch() = %s, expected %s", branch, tt.expected)
			}
		})
	}
}

func TestNewHTTPClientGitHub(t *testing.T) {
	client := NewHTTPClientGitHub("test-token")

	if client.token != "test-token" {
		t.Errorf("expected token 'test-token', got '%s'", client.token)
	}

	if client.baseURL != "https://api.github.com" {
		t.Errorf("expected baseURL 'https://api.github.com', got '%s'", client.baseURL)
	}

	if client.httpClient == nil {
		t.Error("expected httpClient to be initialized")
	}
}

func TestHTTPClientGitHub_GetFileContent_Success(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != http.MethodGet {
			t.Errorf("expected GET method, got %s", r.Method)
		}

		expectedPath := "/repos/myorg/myrepo/contents/path/to/file.yml"
		if r.URL.Path != expectedPath {
			t.Errorf("expected path %s, got %s", expectedPath, r.URL.Path)
		}

		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("expected ref query param 'main', got '%s'", r.URL.Query().Get("ref"))
		}

		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			t.Errorf("expected Accept header 'application/vnd.github.raw+json', got '%s'", r.Header.Get("Accept"))
		}

		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Authorization header 'Bearer test-token', got '%s'", r.Header.Get("Authorization"))
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("file content"))
	}))
	defer server.Close()

	client := &HTTPClientGitHub{
		httpClient: server.Client(),
		token:      "test-token",
		baseURL:    server.URL,
	}

	content, err := client.GetFileContent(context.Background(), "myorg", "myrepo", "path/to/file.yml", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(content) != "file content" {
		t.Errorf("expected content 'file content', got '%s'", string(content))
	}
}

func TestHTTPClientGitHub_GetFileContent_NoRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify no ref query param
		if r.URL.Query().Get("ref") != "" {
			t.Errorf("expected no ref query param, got '%s'", r.URL.Query().Get("ref"))
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("content"))
	}))
	defer server.Close()

	client := &HTTPClientGitHub{
		httpClient: server.Client(),
		token:      "test-token",
		baseURL:    server.URL,
	}

	_, err := client.GetFileContent(context.Background(), "myorg", "myrepo", "file.yml", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClientGitHub_GetFileContent_NoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify no Authorization header
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got '%s'", r.Header.Get("Authorization"))
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("content"))
	}))
	defer server.Close()

	client := &HTTPClientGitHub{
		httpClient: server.Client(),
		token:      "", // No token
		baseURL:    server.URL,
	}

	_, err := client.GetFileContent(context.Background(), "myorg", "myrepo", "file.yml", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClientGitHub_GetFileContent_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := &HTTPClientGitHub{
		httpClient: server.Client(),
		token:      "test-token",
		baseURL:    server.URL,
	}

	_, err := client.GetFileContent(context.Background(), "myorg", "myrepo", "nonexistent.yml", "main")
	if err == nil {
		t.Fatal("expected error for not found")
	}
}

func TestHTTPClientGitHub_GetFileContent_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &HTTPClientGitHub{
		httpClient: server.Client(),
		token:      "test-token",
		baseURL:    server.URL,
	}

	_, err := client.GetFileContent(context.Background(), "myorg", "myrepo", "file.yml", "main")
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestHTTPClientGitHub_GetFileContent_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This won't be called if context is cancelled
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &HTTPClientGitHub{
		httpClient: server.Client(),
		token:      "test-token",
		baseURL:    server.URL,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := client.GetFileContent(ctx, "myorg", "myrepo", "file.yml", "main")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestRepoInfo(t *testing.T) {
	info := RepoInfo{
		Owner:    OwnerInfo{Login: "myorg"},
		Name:     "myrepo",
		FullName: "myorg/myrepo",
	}

	if info.Owner.Login != "myorg" {
		t.Errorf("expected owner login 'myorg', got '%s'", info.Owner.Login)
	}
	if info.Name != "myrepo" {
		t.Errorf("expected name 'myrepo', got '%s'", info.Name)
	}
	if info.FullName != "myorg/myrepo" {
		t.Errorf("expected full name 'myorg/myrepo', got '%s'", info.FullName)
	}
}

func TestCommit_Structure(t *testing.T) {
	commit := Commit{
		ID:       "abc123",
		Added:    []string{"new.go"},
		Modified: []string{"changed.go"},
		Removed:  []string{"deleted.go"},
	}

	if commit.ID != "abc123" {
		t.Errorf("expected ID 'abc123', got '%s'", commit.ID)
	}
	if len(commit.Added) != 1 || commit.Added[0] != "new.go" {
		t.Error("unexpected Added files")
	}
	if len(commit.Modified) != 1 || commit.Modified[0] != "changed.go" {
		t.Error("unexpected Modified files")
	}
	if len(commit.Removed) != 1 || commit.Removed[0] != "deleted.go" {
		t.Error("unexpected Removed files")
	}
}

func TestPushEvent_Structure(t *testing.T) {
	event := PushEvent{
		Ref: "refs/heads/main",
		Repository: RepoInfo{
			Owner:    OwnerInfo{Login: "myorg"},
			Name:     "myrepo",
			FullName: "myorg/myrepo",
		},
		Commits: []Commit{
			{ID: "abc123"},
		},
	}

	if event.Ref != "refs/heads/main" {
		t.Errorf("expected ref 'refs/heads/main', got '%s'", event.Ref)
	}
	if event.Repository.Owner.Login != "myorg" {
		t.Errorf("expected owner 'myorg', got '%s'", event.Repository.Owner.Login)
	}
	if len(event.Commits) != 1 {
		t.Errorf("expected 1 commit, got %d", len(event.Commits))
	}
}

func TestPushEvent_GetModifiedFiles_DeduplicatesAcrossCommits(t *testing.T) {
	event := PushEvent{
		Commits: []Commit{
			{
				Added:    []string{"file1.go"},
				Modified: []string{"file2.go"},
			},
			{
				Added:    []string{"file2.go"}, // Same file added in second commit
				Removed:  []string{"file1.go"}, // Same file removed in second commit
			},
		},
	}

	files := event.GetModifiedFiles()
	if len(files) != 2 {
		t.Errorf("expected 2 unique files, got %d", len(files))
	}
}
