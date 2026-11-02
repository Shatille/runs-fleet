package gocacheprog

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func testID(seed byte) []byte {
	id := make([]byte, HashSize)
	for i := range id {
		id[i] = seed + byte(i)
	}
	return id
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func writeFrames(w io.Writer, frames ...any) error {
	enc := json.NewEncoder(w)
	for _, f := range frames {
		switch v := f.(type) {
		case Request:
			if err := enc.Encode(v); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		case []byte:
			if _, err := io.WriteString(w, `"`+b64(v)+"\"\n"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported frame %T", f)
		}
	}
	return nil
}

func mustWriteFrames(t *testing.T, w io.Writer, frames ...any) {
	t.Helper()
	if err := writeFrames(w, frames...); err != nil {
		t.Fatal(err)
	}
}

func TestFrameReader_GoldenTranscript(t *testing.T) {
	t.Parallel()

	body := []byte("hello")
	outputID := sha256.Sum256(body)
	var buf bytes.Buffer
	mustWriteFrames(t, &buf,
		Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: outputID[:], BodySize: int64(len(body))}, body,
		Request{ID: 2, Command: CmdGet, ActionID: testID(1)},
		Request{ID: 3, Command: CmdClose},
	)

	r := newFrameReader(&buf)

	req, bodyR, err := r.Next()
	if err != nil {
		t.Fatalf("put frame: %v", err)
	}
	if req.ID != 1 || req.Command != CmdPut || req.BodySize != 5 || !bytes.Equal(req.OutputID, outputID[:]) {
		t.Fatalf("put request = %+v", req)
	}
	got, err := io.ReadAll(bodyR)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}

	req, bodyR, err = r.Next()
	if err != nil || req.ID != 2 || req.Command != CmdGet || bodyR != nil {
		t.Fatalf("get frame = %+v, body %v, err %v", req, bodyR, err)
	}
	req, _, err = r.Next()
	if err != nil || req.ID != 3 || req.Command != CmdClose {
		t.Fatalf("close frame = %+v, err %v", req, err)
	}
	if _, _, err = r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after close: err = %v, want EOF", err)
	}
}

func TestFrameReader_RealStreamHasBlankLineAfterEachObject(t *testing.T) {
	t.Parallel()

	body := []byte("x")
	raw := `{"ID":1,"Command":"put","ActionID":"` + b64(testID(1)) + `","OutputID":"` + b64(testID(2)) + `","BodySize":1}` + "\n\n" +
		`"` + b64(body) + `"` + "\n" +
		`{"ID":2,"Command":"get","ActionID":"` + b64(testID(1)) + `"}` + "\n\n" +
		`{"ID":3,"Command":"close"}` + "\n\n"
	r := newFrameReader(strings.NewReader(raw))

	req, bodyR, err := r.Next()
	if err != nil || req.ID != 1 {
		t.Fatalf("frame 1 = %+v, err %v", req, err)
	}
	got, _ := io.ReadAll(bodyR)
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q", got)
	}
	for want := int64(2); want <= 3; want++ {
		req, _, err = r.Next()
		if err != nil || req.ID != want {
			t.Fatalf("frame %d = %+v, err %v", want, req, err)
		}
	}
}

func TestFrameReader_StreamsBodyLargerThanBuffer(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<17)
	pr, pw := io.Pipe()
	go func() {
		err := writeFrames(pw, Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: testID(2), BodySize: int64(len(body))}, body,
			Request{ID: 2, Command: CmdClose})
		pw.CloseWithError(err)
	}()
	r := newFrameReader(pr)
	_, bodyR, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, bodyR)
	if err != nil || n != int64(len(body)) {
		t.Fatalf("copied %d bytes, err %v", n, err)
	}
	if want := sha256.Sum256(body); !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("streamed body differs from source")
	}
	req, _, err := r.Next()
	if err != nil || req.Command != CmdClose {
		t.Fatalf("after body: %+v, %v", req, err)
	}
}

func TestFrameReader_DrainsUnreadBodyBeforeNextFrame(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	mustWriteFrames(t, &buf,
		Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: testID(2), BodySize: 3}, []byte("abc"),
		Request{ID: 2, Command: CmdGet, ActionID: testID(1)},
	)
	r := newFrameReader(&buf)
	if _, _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	req, _, err := r.Next()
	if err != nil || req.ID != 2 {
		t.Fatalf("frame after unread body = %+v, err %v", req, err)
	}
}

func TestFrameReader_PutWithZeroBodyHasNoBodyLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	mustWriteFrames(t, &buf,
		Request{ID: 1, Command: CmdPut, ActionID: testID(1), OutputID: testID(2)},
		Request{ID: 2, Command: CmdClose},
	)
	r := newFrameReader(&buf)
	req, bodyR, err := r.Next()
	if err != nil || bodyR != nil || req.BodySize != 0 {
		t.Fatalf("zero-body put = %+v, body %v, err %v", req, bodyR, err)
	}
	if req, _, err = r.Next(); err != nil || req.ID != 2 {
		t.Fatalf("next = %+v, %v", req, err)
	}
}

func TestFrameReader_Errors(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		raw     string
		wantEOF bool
	}{
		"garbage line":          {raw: "not json\n"},
		"body not quoted":       {raw: `{"ID":1,"Command":"put","ActionID":"` + b64(testID(1)) + `","BodySize":1}` + "\nAA==\n"},
		"truncated body":        {raw: `{"ID":1,"Command":"put","ActionID":"` + b64(testID(1)) + `","BodySize":1}` + "\n\"AA="},
		"empty stream is clean": {raw: "", wantEOF: true},
		"blank lines only":      {raw: "\n\n", wantEOF: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newFrameReader(strings.NewReader(tc.raw))
			_, bodyR, err := r.Next()
			if err == nil && bodyR != nil {
				_, err = io.ReadAll(bodyR)
			}
			if tc.wantEOF {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("err = %v, want EOF", err)
				}
				return
			}
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("err = %v, want a framing error", err)
			}
		})
	}
}

func TestFrameWriter_OneJSONObjectPerLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newFrameWriter(&buf)
	if err := w.Send(Response{ID: 0, KnownCommands: knownCommands}); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(Response{ID: 7, Miss: true}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), buf.String())
	}
	var first Response
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || len(first.KnownCommands) != 3 {
		t.Fatalf("capability line = %q (%v)", lines[0], err)
	}
}
