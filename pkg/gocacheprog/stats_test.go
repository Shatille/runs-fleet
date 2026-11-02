package gocacheprog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStats_SummaryAndOutcomeFileRoundTrip(t *testing.T) {
	t.Parallel()

	var s Stats
	s.Gets.Add(3)
	s.HitsLocal.Add(2)
	s.Misses.Add(1)
	s.Puts.Add(4)
	s.PutBytes.Add(100)
	s.GetBytes.Add(50)
	s.HitsRemote.Add(5)
	s.DownloadBytes.Add(60)
	s.Uploads.Add(6)
	s.UploadBytes.Add(70)
	s.UploadsSkipped.Add(7)
	s.UploadsDropped.Add(8)
	s.UploadsFailed.Add(9)
	s.UploadsAbandoned.Add(10)
	s.RemoteErrors.Add(11)

	path := filepath.Join(t.TempDir(), "outcome")
	if err := AppendOutcome(path, s.Summary()); err != nil {
		t.Fatal(err)
	}
	if err := AppendOutcome(path, Summary{Gets: 1, Errors: 1}); err != nil {
		t.Fatal(err)
	}

	got, err := ReadOutcomes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d summaries, want 2", len(got))
	}
	if got[0] != s.Summary() {
		t.Fatalf("first = %+v, want %+v", got[0], s.Summary())
	}
	if got[0].HitsRemote != 5 || got[0].UploadsAbandoned != 10 || got[0].RemoteErrors != 11 {
		t.Fatalf("remote counters lost: %+v", got[0])
	}
	if got[1].Gets != 1 || got[1].Errors != 1 {
		t.Fatalf("second = %+v", got[1])
	}
}

func TestReadOutcomes_MissingFileIsEmpty(t *testing.T) {
	t.Parallel()

	got, err := ReadOutcomes(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestReadOutcomes_KeepsLinesBeforeADamagedOne(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "outcome")
	if err := os.WriteFile(path, []byte("{\"gets\":3}\n{\"gets\":4}\n{\"ge"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadOutcomes(path)
	if err == nil {
		t.Fatal("damaged tail not reported")
	}
	if len(got) != 2 || got[0].Gets != 3 || got[1].Gets != 4 {
		t.Fatalf("got %+v, want the two intact lines", got)
	}
}

func TestSummary_AddSumsEveryCounter(t *testing.T) {
	t.Parallel()

	a := Summary{Gets: 1, HitsLocal: 2, HitsRemote: 3, Misses: 4, Puts: 5, Errors: 6, GetBytes: 7, PutBytes: 8, DownloadBytes: 9, Uploads: 10, UploadBytes: 11, UploadsSkipped: 12, UploadsDropped: 13, UploadsFailed: 14, UploadsAbandoned: 15, RemoteErrors: 16}
	b := Summary{Gets: 100, HitsLocal: 100, HitsRemote: 100, Misses: 100, Puts: 100, Errors: 100, GetBytes: 100, PutBytes: 100, DownloadBytes: 100, Uploads: 100, UploadBytes: 100, UploadsSkipped: 100, UploadsDropped: 100, UploadsFailed: 100, UploadsAbandoned: 100, RemoteErrors: 100}
	want := Summary{Gets: 101, HitsLocal: 102, HitsRemote: 103, Misses: 104, Puts: 105, Errors: 106, GetBytes: 107, PutBytes: 108, DownloadBytes: 109, Uploads: 110, UploadBytes: 111, UploadsSkipped: 112, UploadsDropped: 113, UploadsFailed: 114, UploadsAbandoned: 115, RemoteErrors: 116}
	if got := a.Add(b); got != want {
		t.Fatalf("Add = %+v, want %+v", got, want)
	}
	if got := (Summary{}).Add(a); got != a {
		t.Fatalf("zero.Add = %+v, want %+v", got, a)
	}
}
