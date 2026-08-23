// Package agentfs provides a transactional semantic index over a real
// filesystem. The filesystem remains the source of truth; the SQLite database
// is a rebuildable index used for relational and full-text queries.
package agentfs
