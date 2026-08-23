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
	// Version-control metadata.
	".git", ".hg", ".svn", ".repo", ".jj",
	// General compiler and build-system caches.
	".cache", ".ccache", ".sccache", ".bazel-cache", "bazel-*", "buck-out",
	// JavaScript and package-manager dependencies/caches and generated sites.
	"node_modules", "bower_components", "jspm_packages", ".npm", ".npm-cache", ".pnpm-store",
	".parcel-cache", ".webpack-cache", ".vite", ".next", ".nuxt", ".svelte-kit", ".angular",
	".docusaurus", ".turbo", ".nx", ".nyc_output", "storybook-static",
	// Python and uv environments, bytecode, test/type-check caches, and package outputs.
	"__pycache__", ".mypy_cache", ".pytest_cache", ".ruff_cache", ".pytype", ".pyre",
	".tox", ".nox", ".hypothesis", ".venv", "venv", ".uv-cache", ".eggs", "*.egg-info",
	"pip-wheel-metadata", ".ipynb_checkpoints",
	// OCaml/Dune local switches and build output; Rust output is under target.
	"_build", "_opam", ".opam-switch", "target",
	// Other common language/build outputs.
	".gradle", ".pants.d", "build", "dist", "out", "coverage", "htmlcov",
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
