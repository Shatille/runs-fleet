// Package gocacheprog implements the GOCACHEPROG protocol that cmd/go speaks to
// an external build-cache helper: JSON requests on stdin, JSON responses on
// stdout, with put bodies streamed as base64 string literals.
package gocacheprog

import "time"

// Cmd is a request type sent by cmd/go.
type Cmd string

const (
	// CmdGet asks for the output stored under an action ID.
	CmdGet Cmd = "get"
	// CmdPut stores an output under an action ID.
	CmdPut Cmd = "put"
	// CmdClose asks the helper to finish and exit.
	CmdClose Cmd = "close"
)

// HashSize is the byte length of action and output IDs (SHA-256).
const HashSize = 32

var knownCommands = []Cmd{CmdGet, CmdPut, CmdClose}

// Request mirrors cmd/go/internal/cacheprog.Request.
type Request struct {
	ID       int64
	Command  Cmd
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

// Response mirrors cmd/go/internal/cacheprog.Response.
type Response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []Cmd      `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}
