package gocacheprog

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const readBufferSize = 64 << 10

var errBodyFraming = errors.New("gocacheprog: put body is not a quoted base64 line")

type frameReader struct {
	br   *bufio.Reader
	body *quotedReader
}

func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{br: bufio.NewReaderSize(r, readBufferSize)}
}

func (f *frameReader) Next() (Request, io.Reader, error) {
	if f.body != nil {
		if err := f.body.discard(); err != nil {
			return Request{}, nil, err
		}
		f.body = nil
	}
	if err := f.skipSpace(); err != nil {
		return Request{}, nil, err
	}
	line, err := f.br.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return Request{}, nil, err
	}
	var req Request
	if uerr := json.Unmarshal(line, &req); uerr != nil {
		return Request{}, nil, fmt.Errorf("gocacheprog: decode request: %w", uerr)
	}
	if req.Command != CmdPut || req.BodySize <= 0 {
		return req, nil, nil
	}
	if serr := f.skipSpace(); serr != nil {
		return Request{}, nil, fmt.Errorf("gocacheprog: body for request %d: %w", req.ID, serr)
	}
	c, rerr := f.br.ReadByte()
	if rerr != nil {
		return Request{}, nil, fmt.Errorf("gocacheprog: body for request %d: %w", req.ID, rerr)
	}
	if c != '"' {
		return Request{}, nil, errBodyFraming
	}
	f.body = &quotedReader{br: f.br}
	return req, base64.NewDecoder(base64.StdEncoding, f.body), nil
}

func (f *frameReader) skipSpace() error {
	for {
		c, err := f.br.ReadByte()
		if err != nil {
			return err
		}
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			return f.br.UnreadByte()
		}
	}
}

type quotedReader struct {
	br   *bufio.Reader
	done bool
}

func (q *quotedReader) Read(p []byte) (int, error) {
	if q.done {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := q.br.Buffered()
	if n == 0 {
		if _, err := q.br.Peek(1); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		n = q.br.Buffered()
	}
	if n > len(p) {
		n = len(p)
	}
	buf, _ := q.br.Peek(n)
	if i := bytes.IndexByte(buf, '"'); i >= 0 {
		copy(p, buf[:i])
		if _, err := q.br.Discard(i + 1); err != nil {
			return i, err
		}
		q.done = true
		q.eatNewline()
		return i, io.EOF
	}
	copy(p, buf)
	if _, err := q.br.Discard(n); err != nil {
		return n, err
	}
	return n, nil
}

func (q *quotedReader) eatNewline() {
	c, err := q.br.ReadByte()
	if err != nil {
		return
	}
	if c != '\n' {
		_ = q.br.UnreadByte()
	}
}

func (q *quotedReader) discard() error {
	_, err := io.Copy(io.Discard, q)
	return err
}

type frameWriter struct {
	mu  sync.Mutex
	bw  *bufio.Writer
	enc *json.Encoder
}

func newFrameWriter(w io.Writer) *frameWriter {
	bw := bufio.NewWriter(w)
	return &frameWriter{bw: bw, enc: json.NewEncoder(bw)}
}

func (w *frameWriter) Send(resp Response) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(resp); err != nil {
		return err
	}
	return w.bw.Flush()
}
