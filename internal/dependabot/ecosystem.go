package dependabot

import (
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// KnownEcosystems is the closed set of package-ecosystem values shuck
// recognizes (GitHub's documented stable ecosystems plus the common beta ones).
// Parse validates config entries against it, and Discover only ever emits these.
var KnownEcosystems = []string{
	"bun",
	"bundler",
	"cargo",
	"composer",
	"devcontainers",
	"docker",
	"docker-compose",
	"dotnet-sdk",
	"elm",
	"github-actions",
	"gitsubmodule",
	"gomod",
	"gradle",
	"helm",
	"maven",
	"mix",
	"npm",
	"nuget",
	"pip",
	"pub",
	"swift",
	"terraform",
	"uv",
	"vcpkg",
}

// Detected is one package ecosystem found in the repository's files, with the
// repo-relative directories (leading "/") whose manifests pointed at it.
type Detected struct {
	Ecosystem   string
	Directories []string
}

// exactManifests maps a manifest's exact base filename to its ecosystem.
var exactManifests = map[string]string{
	"go.mod":           "gomod",
	"package.json":     "npm",
	"requirements.txt": "pip",
	"pipfile":          "pip", // matched lowercased
	"setup.py":         "pip",
	"setup.cfg":        "pip",
	"pyproject.toml":   "pip",
	"composer.json":    "composer",
	"gemfile":          "bundler", // matched lowercased
	"cargo.toml":       "cargo",
	"pom.xml":          "maven",
	"build.gradle":     "gradle",
	"build.gradle.kts": "gradle",
	"settings.gradle":  "gradle",
	"mix.exs":          "mix",
	"pubspec.yaml":     "pub",
	"package.swift":    "swift",
	"elm.json":         "elm",
	"packages.config":  "nuget",
	".gitmodules":      "gitsubmodule",
	"chart.yaml":       "helm",
	"vcpkg.json":       "vcpkg",
	"global.json":      "dotnet-sdk",
	"bun.lockb":        "bun",
	"bun.lock":         "bun",
}

// extManifests maps a manifest's file extension (lowercased, with dot) to its
// ecosystem, for ecosystems identified by extension rather than a fixed name.
var extManifests = map[string]string{
	".gemspec": "bundler",
	".csproj":  "nuget",
	".vbproj":  "nuget",
	".fsproj":  "nuget",
	".nuspec":  "nuget",
	".tf":      "terraform",
}

// Detect maps a repository's file paths (repo-relative, slash-separated) to the
// package ecosystems they imply and the directories those manifests live in.
// The result is sorted by ecosystem, each with a sorted, de-duplicated set of
// directories, so output is deterministic.
func Detect(paths []string) []Detected {
	dirs := map[string]map[string]bool{} // ecosystem -> set of directories
	add := func(eco, dir string) {
		if dirs[eco] == nil {
			dirs[eco] = map[string]bool{}
		}
		dirs[eco][dir] = true
	}
	for _, p := range paths {
		eco, ok := ecosystemForFile(p)
		if !ok {
			continue
		}
		// github-actions is always rooted at "/": Dependabot scans
		// .github/workflows (and a repo's own action manifest) from the repo root.
		if eco == "github-actions" {
			add(eco, "/")
			continue
		}
		add(eco, directoryOf(p))
	}

	out := make([]Detected, 0, len(dirs))
	for eco, set := range dirs {
		ds := make([]string, 0, len(set))
		for d := range set {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		out = append(out, Detected{Ecosystem: eco, Directories: ds})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ecosystem < out[j].Ecosystem })
	return out
}

// ecosystemForFile reports the ecosystem a single repo file implies, if any.
func ecosystemForFile(p string) (string, bool) {
	p = strings.TrimPrefix(p, "./")
	base := path.Base(p)
	lower := strings.ToLower(base)

	// GitHub Actions: workflow files under .github/workflows, or a repo's own
	// composite/JS action manifest anywhere.
	if isWorkflowFile(p) || lower == "action.yml" || lower == "action.yaml" {
		return "github-actions", true
	}
	// Docker: Dockerfile, Dockerfile.<suffix>, or <name>.Dockerfile.
	if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.HasSuffix(base, ".Dockerfile") {
		return "docker", true
	}
	// docker-compose: compose.yaml / docker-compose.yml & friends.
	if isComposeFile(lower) {
		return "docker-compose", true
	}
	if eco, ok := exactManifests[lower]; ok {
		return eco, true
	}
	if eco, ok := extManifests[strings.ToLower(path.Ext(base))]; ok {
		return eco, true
	}
	return "", false
}

// isWorkflowFile reports whether p is a workflow YAML under .github/workflows.
func isWorkflowFile(p string) bool {
	dir := path.Dir(p)
	if dir != ".github/workflows" {
		return false
	}
	ext := strings.ToLower(path.Ext(p))
	return ext == ".yml" || ext == ".yaml"
}

// isComposeFile reports whether a lowercased base filename is a Docker Compose
// file (docker-compose.yml, compose.yaml, and the .override. variants).
func isComposeFile(lower string) bool {
	return slices.Contains([]string{
		"docker-compose.yml", "docker-compose.yaml",
		"docker-compose.override.yml", "docker-compose.override.yaml",
		"compose.yml", "compose.yaml",
		"compose.override.yml", "compose.override.yaml",
	}, lower)
}

// directoryOf returns the repo-relative directory (leading "/", no trailing
// slash) that contains file p, as Dependabot's `directory` expects.
func directoryOf(p string) string {
	return normalizeDir(path.Dir(strings.TrimPrefix(p, "./")))
}

// normalizeDir canonicalizes a directory string to Dependabot's form: a leading
// slash, no trailing slash, "/" for the repo root.
func normalizeDir(d string) string {
	d = strings.TrimSpace(d)
	if d == "" || d == "." || d == "/" {
		return "/"
	}
	d = "/" + strings.Trim(d, "/")
	return d
}

// ecosystems projects detected results onto the report's covered/uncovered view
// given the set of ecosystems the config already manages.
func ecosystems(detected []Detected, covered map[string]bool) []model.DependabotEcosystem {
	out := make([]model.DependabotEcosystem, 0, len(detected))
	for _, d := range detected {
		out = append(out, model.DependabotEcosystem{
			Ecosystem:   d.Ecosystem,
			Directories: d.Directories,
			Covered:     covered[d.Ecosystem],
		})
	}
	return out
}
