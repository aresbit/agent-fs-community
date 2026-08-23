package agentfs

import (
	"errors"
	"time"
)

const (
	defaultContentBytes = 8 * 1024
	defaultExtractBytes = 2 * 1024 * 1024
	defaultMaxRows      = 1_000
)

var (
	ErrClosed             = errors.New("agentfs: store is closed")
	ErrInvalidQuery       = errors.New("agentfs: only read-only SELECT, WITH, EXPLAIN, and PRAGMA queries are allowed")
	ErrNotIndexed         = errors.New("agentfs: path is not indexed")
	ErrUnsafePath         = errors.New("agentfs: operation would affect the index database or a filesystem root")
	ErrIncompatibleSchema = errors.New("agentfs: incompatible database schema")
)

// Options controls index content and query result bounds.
// A zero Options uses an 8 KiB text preview, returns at most 1000 rows, and
// prunes common VCS, cache, dependency, and build-output directories.
type Options struct {
	ContentBytes      int
	ExtractBytes      int
	MaxRows           int
	Embedder          Embedder
	ExcludePatterns   []string
	NoDefaultExcludes bool
}

// ScanOptions controls one complete, transactional root scan.
type ScanOptions struct {
	Tags []string
}

// ScanResult describes a committed scan.
type ScanResult struct {
	Root     string        `json:"root"`
	Entries  int           `json:"entries"`
	Duration time.Duration `json:"duration"`
}

// QueryResult is a bounded SQL result. Rows contain JSON-compatible scalar
// values; byte strings are copied before being returned.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

// CheckReport summarizes database and filesystem consistency checks.
type CheckReport struct {
	Integrity        string   `json:"integrity"`
	FTSIntegrity     string   `json:"fts_integrity"`
	FileRows         int      `json:"file_rows"`
	FTSRows          int      `json:"fts_rows"`
	MissingPaths     []string `json:"missing_paths,omitempty"`
	ForeignKeyErrors []string `json:"foreign_key_errors,omitempty"`
}
