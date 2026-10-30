package gocacheprog

import (
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
