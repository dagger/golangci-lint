package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestEmbedInputs(t *testing.T) {
	data := []byte("package example\n" +
		"//go:test:include test-only.txt\n" +
		"//go:generate:include generator-only.txt\n" +
		"//go:generate go -C ../other run .\n" +
		"// go:embed ignored.txt\n" +
		"//go:embedded ignored-too.txt\n" +
		"/* go:embed ignored-block.txt */\n" +
		"//go:embed assets/*.txt all:hidden \"space name.txt\" `raw name.txt`\n" +
		"var assets string\n")
	directives, err := goDirectivesInFile("pkg/embed_test.go", data)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, directive := range directives {
		patterns, err := directive.includePatterns()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, patterns...)
	}
	want := []string{"pkg/assets/*.txt", "pkg/hidden", "pkg/space name.txt", "pkg/raw name.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestInvalidEmbedQuote(t *testing.T) {
	_, err := (goDirective{position: "main.go:2:1", comment: `//go:embed "unterminated`}).includePatterns()
	if err == nil || !strings.Contains(err.Error(), "main.go:2:1") {
		t.Fatalf("expected error with source position, got %v", err)
	}
}

func TestModuleIsolationAndTransitiveReplaces(t *testing.T) {
	root := fixture(t, map[string]string{
		"app/go.mod":         "module example.com/app\nreplace example.com/lib => ../lib\n",
		"app/main.go":        "package app\n//go:embed app.txt\nvar data string\n",
		"app/nested/go.mod":  "module example.com/nested\n",
		"app/nested/main.go": "package nested\n//go:embed nested-only.txt\nvar data string\n",
		"lib/go.mod":         "module example.com/lib\nreplace example.com/leaf => ../leaf\n",
		"lib/main.go":        "package lib\n//go:embed lib.txt\nvar data string\n",
		"leaf/go.mod":        "module example.com/leaf\nreplace example.com/app => ../app\n",
		"leaf/main.go":       "package leaf\n//go:embed leaf.txt\nvar data string\n",
		"unrelated/go.mod":   "module example.com/unrelated\n",
		"unrelated/main.go":  "package unrelated\n//go:embed unrelated.txt\nvar data string\n",
	})
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := index.includesFor("app")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app/app.txt", "lib/lib.txt", "leaf/leaf.txt", "app/**/go.mod", "app/**/*.cc"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	for _, unwanted := range []string{"app/nested/nested-only.txt", "unrelated/unrelated.txt"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("another module's directive leaked into app: %s", unwanted)
		}
	}
	seen := map[string]bool{}
	for _, pattern := range got {
		if seen[pattern] {
			t.Errorf("duplicate pattern after replacement cycle: %s", pattern)
		}
		seen[pattern] = true
	}
}

func TestMissingLocalReplace(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module example.com/app\nreplace example.com/missing => ./missing\n",
	})
	index, err := indexLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = index.includesFor(".")
	if err == nil || !strings.Contains(err.Error(), "local replace target: ./missing") {
		t.Fatalf("expected missing replace error, got %v", err)
	}
}

func TestRunWritesRootAndNestedModules(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod":        "module example.com/root\n",
		"nested/go.mod": "module example.com/nested\n",
	})
	output := t.TempDir()
	if err := run([]string{"--root", root, "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"_root_.inc": "go.mod\n", "nested.inc": "nested/go.mod\n"} {
		data, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("%s missing %q", name, want)
		}
	}
}

func TestRunRequiresOutput(t *testing.T) {
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "--output-dir is required") {
		t.Fatalf("expected missing output error, got %v", err)
	}
}
