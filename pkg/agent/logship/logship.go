// Package logship uploads a runner's _diag logs to S3 before the agent wipes
// the runner directory, so a job stays diagnosable after GitHub expires its own
// logs (superseded attempts return BlobNotFound within hours).
package logship

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Ship outcome values, mirroring pkg/agent's BuildCache* discipline.
const (
	OutcomeUploaded = "uploaded"
	OutcomePartial  = "partial"
	OutcomeFailed   = "failed"
	OutcomeSkipped  = "skipped"
	OutcomeDisabled = "disabled"
)

const (
	// DefaultPrefix keys logs beside the Actions cache's caches/ and the buildx
	// shim's buildkit/ in the same bucket.
	DefaultPrefix = "runner-logs/"

	// unknownJobSegment keeps a key well-formed when the runner config carried no
	// job_id; the run-level prefix still lists the object.
	unknownJobSegment = "unknown-job"
)

// PutObjectAPI is the S3 surface logship needs, narrowed for test injection.
type PutObjectAPI interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Logger is the subset of the agent's logger this package uses.
type Logger interface {
	Printf(format string, v ...any)
}

// Config identifies the job whose logs are being shipped and bounds the work.
// An empty Bucket disables shipping entirely.
type Config struct {
	Bucket       string
	Prefix       string
	RunID        string
	JobID        string
	InstanceID   string
	Repo         string
	MaxFileBytes int64
	Timeout      time.Duration
}

// Shipper uploads _diag logs for one job.
type Shipper struct {
	s3     PutObjectAPI
	cfg    Config
	logger Logger
}

// New builds a Shipper against the ambient AWS credentials.
func New(awsCfg aws.Config, cfg Config, logger Logger) *Shipper {
	return NewWithClient(s3.NewFromConfig(awsCfg), cfg, logger)
}

// NewWithClient builds a Shipper around an injected S3 client.
func NewWithClient(client PutObjectAPI, cfg Config, logger Logger) *Shipper {
	return &Shipper{s3: client, cfg: cfg, logger: logger}
}

// BuildKey returns the object key for one log file. Readers derive the same key
// from a GitHub job URL, so this is the single definition of the layout.
func BuildKey(prefix, runID, jobID, instanceID, name string) string {
	return BuildPrefix(prefix, runID, jobID) + instanceID + "/" + name
}

// BuildPrefix returns the listable prefix for a run, or for one job within it
// when jobID is non-empty.
func BuildPrefix(prefix, runID, jobID string) string {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if jobID == "" {
		return prefix + runID + "/"
	}
	return prefix + runID + "/" + jobID + "/"
}

// Ship uploads every _diag log for the job and reports an outcome. It never
// returns an error: a failed upload must not fail the job, and a slow one must
// not delay self-termination, so callers only record what happened.
func (s *Shipper) Ship(ctx context.Context, runnerPath string) string {
	if s.cfg.Bucket == "" {
		return OutcomeDisabled
	}

	files, err := diagLogs(runnerPath)
	if err != nil || len(files) == 0 {
		return OutcomeSkipped
	}

	if s.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()
	}

	var uploaded, skipped, failed int
	for _, path := range files {
		switch s.shipOne(ctx, path) {
		case OutcomeUploaded:
			uploaded++
		case OutcomeSkipped:
			skipped++
		default:
			failed++
		}
	}

	switch {
	case uploaded == 0:
		return OutcomeFailed
	case failed > 0 || skipped > 0:
		return OutcomePartial
	default:
		return OutcomeUploaded
	}
}

func (s *Shipper) shipOne(ctx context.Context, path string) string {
	info, statErr := os.Stat(path)
	if statErr != nil {
		s.logf("runner log %s unreadable: %v", filepath.Base(path), statErr)
		return OutcomeFailed
	}
	if s.cfg.MaxFileBytes > 0 && info.Size() > s.cfg.MaxFileBytes {
		s.logf("runner log %s is %d bytes, over the %d cap; skipping", filepath.Base(path), info.Size(), s.cfg.MaxFileBytes)
		return OutcomeSkipped
	}

	body, gzErr := gzipFile(path)
	if gzErr != nil {
		s.logf("runner log %s could not be compressed: %v", filepath.Base(path), gzErr)
		return OutcomeFailed
	}

	key := BuildKey(s.cfg.Prefix, s.cfg.RunID, s.jobSegment(), s.cfg.InstanceID, filepath.Base(path)+".gz")
	_, putErr := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               aws.String(s.cfg.Bucket),
		Key:                  aws.String(key),
		Body:                 bytes.NewReader(body),
		ContentType:          aws.String("text/plain"),
		ContentEncoding:      aws.String("gzip"),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
		Metadata:             s.metadata(),
	})
	if putErr != nil {
		s.logf("runner log %s upload failed: %v", filepath.Base(path), putErr)
		return OutcomeFailed
	}
	return OutcomeUploaded
}

func (s *Shipper) jobSegment() string {
	if s.cfg.JobID == "" {
		return unknownJobSegment
	}
	return s.cfg.JobID
}

func (s *Shipper) metadata() map[string]string {
	meta := map[string]string{}
	if s.cfg.Repo != "" {
		meta["repo"] = s.cfg.Repo
	}
	if s.cfg.JobID != "" {
		meta["job-id"] = s.cfg.JobID
	}
	if s.cfg.RunID != "" {
		meta["run-id"] = s.cfg.RunID
	}
	return meta
}

func (s *Shipper) logf(format string, v ...any) {
	if s.logger != nil {
		s.logger.Printf(format, v...)
	}
}

func diagLogs(runnerPath string) ([]string, error) {
	diag := filepath.Join(runnerPath, "_diag")
	if _, statErr := os.Stat(diag); statErr != nil {
		return nil, fmt.Errorf("stat _diag: %w", statErr)
	}
	var files []string
	for _, pattern := range []string{"Worker_*.log", "Runner_*.log"} {
		matches, globErr := filepath.Glob(filepath.Join(diag, pattern))
		if globErr != nil {
			return nil, fmt.Errorf("glob %s: %w", pattern, globErr)
		}
		files = append(files, matches...)
	}
	return files, nil
}

func gzipFile(path string) ([]byte, error) {
	src, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = src.Close() }()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, copyErr := io.Copy(zw, src); copyErr != nil {
		return nil, fmt.Errorf("compress: %w", copyErr)
	}
	if closeErr := zw.Close(); closeErr != nil {
		return nil, fmt.Errorf("finish gzip: %w", closeErr)
	}
	return buf.Bytes(), nil
}
