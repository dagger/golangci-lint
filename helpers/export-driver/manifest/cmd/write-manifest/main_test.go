package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestWriteManifest checks the manifest written for a listing's archives:
// the settings from go env and the tags, and for each archived package its
// archive and digest, and for a cgo package its cgo files, cgo's output and
// its module's version, the replacement's.
func TestWriteManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	files := map[string]string{
		"p/a.go":       "package p\n",
		"p/a_test.go":  "package p\n\nfunc f() {}\n",
		"p/c.go":       "package p\n\nimport \"C\"\n",
		"p/x.c":        "int x;\n",
		"p/data/e.txt": "embedded\n",
		"q/q.go":       "package q\n",
	}
	for name, data := range files {
		write("src/"+name, data)
	}
	gen := map[string]string{
		"_cgo_gotypes.go": "//line /somewhere/c.go:1:1\npackage p\n",
		"c.cgo1.go":       "package p\n//line /somewhere/c.go:3:1\nvar _ = 1 // line, not a directive\n",
	}
	for name, data := range gen {
		write("archives-dir/cgo/p/"+name, data)
	}
	src := filepath.Join(dir, "src")
	var list bytes.Buffer
	enc := json.NewEncoder(&list)
	for _, p := range []map[string]any{
		{"ImportPath": "example.com/p", "Module": map[string]any{"Path": "example.com/p", "Version": "v1.0.0", "Replace": map[string]any{"Path": "example.com/fork", "Version": "v1.0.1"}}, "Dir": src + "/p", "GoFiles": []string{"a.go"}, "CgoFiles": []string{"c.go"}, "CFiles": []string{"x.c"}, "EmbedFiles": []string{"data/e.txt"}},
		{"ImportPath": "example.com/p [example.com/p.test]", "ForTest": "example.com/p", "Dir": src + "/p", "GoFiles": []string{"a.go", "a_test.go"}},
		{"ImportPath": "example.com/p.test", "Dir": src + "/p", "GoFiles": []string{"_testmain.go"}},
		{"ImportPath": "example.com/q", "Dir": src + "/q", "GoFiles": []string{"q.go"}},
	} {
		if err := enc.Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	listPath := write("list.json", list.String())
	env := map[string]string{"GOVERSION": "go1.26.1", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1", "CC": "", "CXX": "", "CGO_CFLAGS": "-O2", "CGO_CPPFLAGS": "", "CGO_CXXFLAGS": "", "CGO_FFLAGS": "", "CGO_LDFLAGS": "", "GOMODCACHE": src}
	entriesPath := write("entries.txt", "example.com/p\tarchives/p.a\tcgo/p/_cgo_gotypes.go\tcgo/p/c.cgo1.go\n"+
		"example.com/p [example.com/p.test]\tarchives/pt.a\nexample.com/q\tarchives/q.a\n")
	var out bytes.Buffer
	if err := run(context.Background(), listPath, entriesPath, "b,a", filepath.Join(dir, "archives-dir"), env, &out); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("manifest: %v\n%s", err, out.String())
	}
	digest := func(lines ...string) string {
		sort.Strings(lines)
		return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(strings.Join(lines, ""))))
	}
	line := func(name, data string) string { return fmt.Sprintf("%s %x\n", name, sha256.Sum256([]byte(data))) }
	want := map[string]any{
		"version": 2.0, "goVersion": "go1.26.1", "goos": "linux", "goarch": "amd64", "cgoEnabled": true,
		"buildTags": []any{"a", "b"},
		"cgo":       map[string]any{"CC": "", "CXX": "", "CGO_CFLAGS": "-O2", "CGO_CPPFLAGS": "", "CGO_CXXFLAGS": "", "CGO_FFLAGS": "", "CGO_LDFLAGS": ""},
		"packages": map[string]any{
			"example.com/p": map[string]any{"archive": "archives/p.a", "sources": digest(
				"cgo\n",
				line("cgo-output-0", "\npackage p\n"),
				line("cgo-output-1", "package p\n\nvar _ = 1 // line, not a directive\n"),
				line("a.go", files["p/a.go"]),
				line("c.go", files["p/c.go"]),
				line("x.c", files["p/x.c"]),
				line("data/e.txt", files["p/data/e.txt"]),
			),
				"cgoFiles":  []any{"c.go"},
				"module":    "example.com/fork@v1.0.1",
				"cgoOutput": []any{"cgo/p/_cgo_gotypes.go", "cgo/p/c.cgo1.go"},
			},
			"example.com/p [example.com/p.test]": map[string]any{"archive": "archives/pt.a", "sources": digest(
				line("a.go", files["p/a.go"]),
				line("a_test.go", files["p/a_test.go"]),
			)},
			"example.com/q": map[string]any{"archive": "archives/q.a", "sources": digest(line("q.go", files["q/q.go"]))},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifest:\n%s\nwant:\n%v", out.String(), want)
	}
}

// TestWriteManifestRefuses checks that an entry for a package the listing
// lacks, and a cgo package without cgo's output, are errors.
func TestWriteManifestRefuses(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := os.WriteFile(filepath.Join(dir, "c.go"), []byte("package c\n\nimport \"C\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	listPath := write("list.json", `{"ImportPath": "example.com/c", "Dir": "`+dir+`", "CgoFiles": ["c.go"]}`)
	env := map[string]string{"GOVERSION": "go1.26.1", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
	for name, entries := range map[string]string{
		"unlisted":        "example.com/other\tarchives/o.a\n",
		"no cgo's output": "example.com/c\tarchives/c.a\n",
	} {
		entriesPath := write("entries.txt", entries)
		if err := run(context.Background(), listPath, entriesPath, "", dir, env, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
