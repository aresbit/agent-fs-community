package agentfs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// defaultExcludePatterns contains generated, cached, dependency, and VCS
// directory basenames that should not consume parser work or filesystem
// watches in a local source-context index. Patterns use filepath.Match and are
// intentionally matched against one path component, not an absolute path.
var defaultExcludePatterns = []string{
	".git", ".hg", ".svn", ".repo", ".jj",
	".cache", ".ccache", ".sccache", ".bazel-cache", "bazel-*", "buck-out",
	"node_modules", ".yarn", ".pnpm-store",
	"__pycache__", ".mypy_cache", ".pytest_cache", ".ruff_cache", ".tox", ".venv", "venv",
	".gradle", ".next", ".nuxt", ".pants.d", ".turbo", ".nx",
	"build", "dist", "out", "target", "coverage", "htmlcov",
	"DerivedData", "Pods",
}

// DefaultExcludePatterns returns the basename globs excluded by a zero-value
// Options. The returned slice is safe for the caller to modify.
func DefaultExcludePatterns() []string {
	return slices.Clone(defaultExcludePatterns)
}

func buildExcludePatterns(custom []string, noDefaults bool) ([]string, error) {
	patterns := make([]string, 0, len(defaultExcludePatterns)+len(custom))
	if !noDefaults {
		patterns = append(patterns, defaultExcludePatterns...)
	}
	patterns = append(patterns, custom...)
	seen := make(map[string]struct{}, len(patterns)+len(custom))
	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, fmt.Errorf("exclude pattern is empty: %w", os.ErrInvalid)
		}
		if strings.ContainsAny(pattern, `/\\`) {
			return nil, fmt.Errorf("exclude pattern %q must match one path basename: %w", pattern, os.ErrInvalid)
		}
		if _, err := filepath.Match(pattern, "agentfs-validation"); err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %v: %w", pattern, err, os.ErrInvalid)
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		normalized = append(normalized, pattern)
	}
	return normalized, nil
}

func (s *Store) isExcludedName(name string) bool {
	for _, pattern := range s.excludePatterns {
		matched, _ := filepath.Match(pattern, name)
		if matched {
			return true
		}
	}
	return false
}

// isExcludedWithin reports whether a path below root crosses an excluded path
// component. The explicitly configured root itself is always eligible even if
// its basename happens to match a default pattern.
func (s *Store) isExcludedWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." {
		return false
	}
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component != "." && component != ".." && s.isExcludedName(component) {
			return true
		}
	}
	return false
}
