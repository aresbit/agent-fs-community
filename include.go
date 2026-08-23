package agentfs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// defaultIncludeExtensions is an allowlist of source, Markdown, infrastructure,
// and text configuration formats. Binary media, archives, databases, generated
// artifacts, PDFs, and Office documents are intentionally absent.
var defaultIncludeExtensions = []string{
	// Documentation and web source.
	".md", ".markdown", ".mdx", ".html", ".htm", ".css", ".scss", ".sass", ".less",
	// Go, Python, JavaScript/TypeScript, Rust.
	".go", ".py", ".pyi", ".pyx", ".pxd", ".pxi",
	".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts", ".vue", ".svelte",
	".rs",
	// C-family, JVM, Apple, and .NET.
	".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx", ".m", ".mm",
	".cu", ".cuh", ".cl", ".java", ".kt", ".kts", ".scala", ".sc", ".groovy", ".swift",
	".cs", ".fs", ".fsx", ".fsi", ".vb",
	// OCaml/Reason, BEAM, functional and scripting languages.
	".ml", ".mli", ".re", ".rei", ".hs", ".lhs", ".erl", ".hrl", ".ex", ".exs",
	".clj", ".cljs", ".cljc", ".edn", ".rb", ".rake", ".php", ".lua", ".r", ".jl",
	".dart", ".zig", ".nim",
	// Hardware, assembly, shells, schemas and smart contracts.
	".v", ".sv", ".svh", ".vhd", ".vhdl", ".asm", ".s", ".wat",
	".sh", ".bash", ".zsh", ".fish", ".ps1", ".bat", ".cmd",
	".sql", ".proto", ".thrift", ".graphql", ".gql", ".sol",
	// Infrastructure, build, and text configuration.
	".tf", ".tfvars", ".hcl", ".cue", ".nix",
	".yaml", ".yml", ".toml", ".json", ".jsonc", ".xml", ".ini", ".cfg", ".conf",
	".properties", ".gradle", ".bzl", ".bazel", ".cmake", ".mk", ".opam", ".plist",
}

var defaultIncludeExtensionSet = func() map[string]struct{} {
	values := make(map[string]struct{}, len(defaultIncludeExtensions))
	for _, extension := range defaultIncludeExtensions {
		values[extension] = struct{}{}
	}
	return values
}()

// defaultIncludePatterns covers important source/build files without a useful
// extension. Patterns are matched against one basename with filepath.Match.
var defaultIncludePatterns = []string{
	"Dockerfile", "Dockerfile.*", "Containerfile", "Containerfile.*",
	"Makefile", "makefile", "GNUmakefile", "CMakeLists.txt", "meson.build", "meson_options.txt",
	"BUILD", "BUILD.*", "WORKSPACE", "WORKSPACE.*", "MODULE.bazel",
	"Justfile", "Taskfile.yml", "Taskfile.yaml", "Tiltfile", "Procfile",
	"go.mod", "go.sum", "go.work", "go.work.sum",
	"Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml",
	"dune", "dune-project", "dune-workspace",
	"pyproject.toml", "requirements*.txt", "constraints*.txt", "Pipfile", "Pipfile.lock",
	"poetry.lock", "uv.lock", "setup.py", "setup.cfg", "tox.ini",
	"package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock",
	"deno.json", "deno.jsonc", "tsconfig*.json", "jsconfig*.json",
	"Gemfile", "Gemfile.lock", "Rakefile", "Podfile", "Podfile.lock",
	".gitignore", ".gitattributes", ".gitmodules", ".editorconfig", ".dockerignore",
	".npmrc", ".nvmrc", ".python-version", ".tool-versions", ".env.example", ".env.sample",
	"configure", "gradlew", "mvnw", "waf",
}

// DefaultIncludeExtensions returns the lower-case file extensions admitted by
// a zero-value Options. The returned slice is safe for callers to modify.
func DefaultIncludeExtensions() []string {
	return slices.Clone(defaultIncludeExtensions)
}

// DefaultIncludePatterns returns extensionless/special basename globs admitted
// by a zero-value Options. The returned slice is safe for callers to modify.
func DefaultIncludePatterns() []string {
	return slices.Clone(defaultIncludePatterns)
}

func buildIncludePatterns(custom []string) (map[string]struct{}, []string, error) {
	patterns := make([]string, 0, len(defaultIncludePatterns)+len(custom))
	patterns = append(patterns, defaultIncludePatterns...)
	patterns = append(patterns, custom...)
	seen := make(map[string]struct{}, len(patterns))
	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || strings.ContainsAny(pattern, `/\\`) {
			return nil, nil, fmt.Errorf("include-file pattern %q must be a non-empty basename glob: %w",
				pattern, os.ErrInvalid)
		}
		if _, err := filepath.Match(pattern, "agentfs-validation"); err != nil {
			return nil, nil, fmt.Errorf("invalid include-file pattern %q: %v: %w", pattern, err, os.ErrInvalid)
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		normalized = append(normalized, pattern)
	}
	names := make(map[string]struct{}, len(normalized))
	globs := make([]string, 0, len(normalized))
	for _, pattern := range normalized {
		if strings.ContainsAny(pattern, "*?[") {
			globs = append(globs, pattern)
		} else {
			names[pattern] = struct{}{}
		}
	}
	return names, globs, nil
}

func (s *Store) isIncludedFileName(name string) bool {
	if s.allFiles {
		return true
	}
	extension := strings.ToLower(filepath.Ext(name))
	if _, ok := defaultIncludeExtensionSet[extension]; ok {
		return true
	}
	if _, ok := s.includeNames[name]; ok {
		return true
	}
	for _, pattern := range s.includePatterns {
		matched, _ := filepath.Match(pattern, name)
		if matched {
			return true
		}
	}
	return false
}
