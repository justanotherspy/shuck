package dependabot

import (
	"reflect"
	"testing"
)

func TestEcosystemForFile(t *testing.T) {
	tests := []struct {
		path string
		want string
		ok   bool
	}{
		{"go.mod", "gomod", true},
		{"sub/dir/go.mod", "gomod", true},
		{"frontend/package.json", "npm", true},
		{"requirements.txt", "pip", true},
		{"pyproject.toml", "pip", true},
		{"Pipfile", "pip", true},
		{"composer.json", "composer", true},
		{"Gemfile", "bundler", true},
		{"lib.gemspec", "bundler", true},
		{"Cargo.toml", "cargo", true},
		{"pom.xml", "maven", true},
		{"build.gradle.kts", "gradle", true},
		{"mix.exs", "mix", true},
		{"pubspec.yaml", "pub", true},
		{"Package.swift", "swift", true},
		{"elm.json", "elm", true},
		{"App.csproj", "nuget", true},
		{"packages.config", "nuget", true},
		{".gitmodules", "gitsubmodule", true},
		{"chart/Chart.yaml", "helm", true},
		{"vcpkg.json", "vcpkg", true},
		{"global.json", "dotnet-sdk", true},
		{"bun.lockb", "bun", true},
		{"infra/main.tf", "terraform", true},
		{"Dockerfile", "docker", true},
		{"Dockerfile.prod", "docker", true},
		{"build.Dockerfile", "docker", true},
		{"docker-compose.yml", "docker-compose", true},
		{"compose.yaml", "docker-compose", true},
		{".github/workflows/ci.yml", "github-actions", true},
		{".github/workflows/release.yaml", "github-actions", true},
		{"action.yml", "github-actions", true},
		{"my-action/action.yaml", "github-actions", true},
		{"README.md", "", false},
		{"src/main.go", "", false},
		{".github/dependabot.yml", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, ok := ecosystemForFile(tt.path)
			if ok != tt.ok || got != tt.want {
				t.Errorf("ecosystemForFile(%q) = (%q, %v), want (%q, %v)", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNormalizeDir(t *testing.T) {
	tests := map[string]string{
		"":           "/",
		".":          "/",
		"/":          "/",
		"sub":        "/sub",
		"/sub/":      "/sub",
		"a/b/c":      "/a/b/c",
		"  /trim/  ": "/trim",
	}
	for in, want := range tests {
		if got := normalizeDir(in); got != want {
			t.Errorf("normalizeDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetect(t *testing.T) {
	paths := []string{
		"go.mod",
		"frontend/package.json",
		"frontend/src/index.js",
		"services/api/package.json",
		".github/workflows/ci.yml",
		".github/workflows/release.yml",
		"Dockerfile",
		"README.md",
	}
	got := Detect(paths)
	want := []Detected{
		{Ecosystem: "docker", Directories: []string{"/"}},
		{Ecosystem: "github-actions", Directories: []string{"/"}},
		{Ecosystem: "gomod", Directories: []string{"/"}},
		{Ecosystem: "npm", Directories: []string{"/frontend", "/services/api"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Detect mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestDetectEmpty(t *testing.T) {
	if got := Detect(nil); len(got) != 0 {
		t.Errorf("Detect(nil) = %v, want empty", got)
	}
	if got := Detect([]string{"README.md", "LICENSE"}); len(got) != 0 {
		t.Errorf("Detect(no manifests) = %v, want empty", got)
	}
}

func TestDirectoryOf(t *testing.T) {
	if got := directoryOf("./a/go.mod"); got != "/a" {
		t.Errorf("directoryOf = %q, want /a", got)
	}
	if got := directoryOf("go.mod"); got != "/" {
		t.Errorf("directoryOf root = %q, want /", got)
	}
}
