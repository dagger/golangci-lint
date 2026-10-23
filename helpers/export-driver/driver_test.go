package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// golangciMode is the load mode golangci-lint v2 requests for type-aware
// linters (8767).
const golangciMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedExportFile | packages.NeedTypesSizes | packages.NeedModule

// golangciFlags are the build flags golangci-lint v2 passes by default.
var golangciFlags = []string{"-buildvcs=false"}

// writeModule writes a module where:
//   - lib is a dependency with a deprecated function;
//   - app imports lib and the standard library;
//   - app has internal tests, and its external test imports helper, which
//     imports app, so the test build has the variant
//     "helper [example.com/m/app.test]", compiled against app's test variant.
func writeModule(t *testing.T, goVersion string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":               "module example.com/m\n\ngo " + goVersion + "\n",
		"lib/lib.go":           "package lib\n\n// Old is old.\n//\n// Deprecated: use New.\nfunc Old() int { return 1 }\n\n// New is new.\nfunc New() int { return 2 }\n",
		"app/app.go":           "package app\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/m/lib\"\n)\n\n// Use uses lib.\nfunc Use() string { return fmt.Sprint(lib.Old()) }\n",
		"helper/helper.go":     "package helper\n\nimport \"example.com/m/app\"\n\n// Call calls app.\nfunc Call() string { return app.Use() }\n",
		"lib/lib_test.go":      "package lib\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n)\n\n// Only the test embeds: go/packages leaves its pattern out.\n//\n//go:embed lib.go\nvar src string\n\nfunc TestOld(t *testing.T) { _ = Old(); _ = src }\n",
		"app/app_test.go":      "package app_test\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n\n\t\"example.com/m/helper\"\n)\n\n//go:embed data/msg.txt\nvar extMsg string\n\nfunc TestCall(t *testing.T) { _ = helper.Call(); _ = extMsg }\n",
		"app/internal_test.go": "package app\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n)\n\nvar (\n\t//go:embed data/msg.txt\n\twantMsg string\n)\n\nfunc TestUse(t *testing.T) { _ = Use(); _ = wantMsg }\n",
		"app/embed.go":         "package app\n\nimport _ \"embed\"\n\n//go:embed data/msg.txt\nvar msg string\n\n// Msg returns the embedded message.\nfunc Msg() string { return msg }\n",
		"app/data/msg.txt":     "hello\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func toolchainLang() string { return langVersion(runtime.Version()) }

// archivesFor writes the manifest directory for the module's packages with
// tests: the stand-in for a per-package builder.
func archivesFor(t *testing.T, mod string) string {
	t.Helper()
	out := t.TempDir()
	t.Chdir(mod)
	if err := writeManifestDir(context.Background(), out, []string{"--", "./..."}); err != nil {
		t.Fatal(err)
	}
	return out
}

func serveModule(t *testing.T, mod, archives string, mutate func(*packages.DriverRequest)) (*packages.DriverResponse, report) {
	t.Helper()
	t.Chdir(mod)
	t.Setenv(envArchives, archives)
	t.Setenv(envWorkDir, t.TempDir())
	linterIs(t, runtime.Version())
	req := &packages.DriverRequest{Mode: golangciMode, Env: os.Environ(), Tests: true, BuildFlags: golangciFlags}
	if mutate != nil {
		mutate(req)
	}
	resp, rep, err := serve(context.Background(), req, []string{"./..."})
	if err != nil {
		t.Fatal(err)
	}
	return resp, rep
}

func rootsOf(resp *packages.DriverResponse) map[string]bool {
	roots := map[string]bool{}
	for _, r := range resp.Roots {
		roots[r] = true
	}
	return roots
}

// TestServesFilesAndExportData checks the two ways linting through a driver
// can silently find nothing: a root without files to parse, or a dependency
// without export data.
func TestServesFilesAndExportData(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	archives := archivesFor(t, mod)
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	roots := rootsOf(resp)
	if len(roots) == 0 {
		t.Fatal("no roots")
	}
	var sawVariant bool
	for _, p := range resp.Packages {
		if roots[p.ID] && len(p.CompiledGoFiles) == 0 {
			t.Errorf("root %s has no CompiledGoFiles: golangci-lint would lint nothing", p.ID)
		}
		if p.PkgPath == "unsafe" {
			continue
		}
		if roots[p.ID] && p.ID != p.PkgPath {
			continue // a root test variant, type-checked from source
		}
		if isTestMain(p) {
			continue // a generated test main, which golangci-lint skips
		}
		// Every other package may be loaded from export data: golangci-lint
		// lints a package with internal tests through its test variant and
		// loads the plain package from export data.
		if p.ExportFile == "" {
			t.Errorf("dependency %s has no export data", p.ID)
		} else if _, err := os.Stat(p.ExportFile); err != nil {
			t.Errorf("dependency %s: %v", p.ID, err)
		}
		if strings.HasPrefix(p.ID, "example.com/m/helper [") {
			sawVariant = true
		}
	}
	if !sawVariant {
		t.Error("the test variant of helper is not a dependency; the fixture no longer covers compiling one")
	}
	if rep.Prebuilt == 0 || rep.Compiled == 0 || rep.TestsCompiled == 0 || rep.GoList != 0 || len(rep.Missing) != 0 || len(rep.CompileErrors) != 0 {
		t.Errorf("report = %+v, want prebuilt and compiled archives only", rep)
	}
}

// TestStaleArchiveIsNotUsed checks that an archive whose sources changed is
// never served, nor any archive compiled against it.
func TestStaleArchiveIsNotUsed(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	archives := archivesFor(t, mod)
	var m manifest
	data, err := os.ReadFile(filepath.Join(archives, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(mod, "lib", "lib.go")
	src, err := os.ReadFile(lib)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lib, append(src, []byte("\n// Newer is newer.\nfunc Newer() int { return 3 }\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	stale := map[string]bool{}
	for path, e := range m.Packages {
		if strings.HasPrefix(path, "example.com/m/") {
			stale[filepath.Join(archives, e.Archive)] = true
		}
	}
	for _, p := range resp.Packages {
		if stale[p.ExportFile] {
			t.Errorf("%s uses an archive built from old sources", p.ID)
		}
	}
	if rep.Prebuilt == 0 {
		t.Errorf("report = %+v: the unchanged standard library should still be prebuilt", rep)
	}
}

func TestDeclines(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	archives := archivesFor(t, mod)

	editManifest := func(t *testing.T, edit func(*manifest)) string {
		data, err := os.ReadFile(filepath.Join(archives, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		edit(&m)
		dir := t.TempDir()
		for path, e := range m.Packages {
			e.Archive = filepath.Join(archives, e.Archive)
			m.Packages[path] = e
		}
		out, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), out, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	cases := []struct {
		name     string
		archives func(t *testing.T) string
		mutate   func(*packages.DriverRequest)
		linter   string
		module   string
	}{
		{name: "no archives", archives: func(*testing.T) string { return "" }},
		{name: "other toolchain", archives: func(t *testing.T) string {
			return editManifest(t, func(m *manifest) { m.GoVersion = "go1.1" })
		}},
		{name: "other platform", archives: func(t *testing.T) string {
			return editManifest(t, func(m *manifest) { m.GOARCH = "other" })
		}},
		{name: "other tags", archives: func(t *testing.T) string { return archives },
			mutate: func(r *packages.DriverRequest) { r.BuildFlags = []string{"-tags", "extra"} }},
		{name: "race", archives: func(t *testing.T) string { return archives },
			mutate: func(r *packages.DriverRequest) { r.BuildFlags = []string{"-race"} }},
		{name: "overlay", archives: func(t *testing.T) string { return archives },
			mutate: func(r *packages.DriverRequest) { r.Overlay = map[string][]byte{"x.go": nil} }},
		{name: "linter built with another Go", archives: func(t *testing.T) string { return archives },
			linter: "go1.1.0"},
		{name: "module on an older Go", archives: func(t *testing.T) string { return archives }, module: "1.22"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.archives(t)
			m := mod
			if tc.module != "" {
				m = writeModule(t, tc.module)
			}
			t.Chdir(m)
			t.Setenv(envArchives, dir)
			t.Setenv(envWorkDir, t.TempDir())
			linterIs(t, runtime.Version())
			if tc.linter != "" {
				linterIs(t, tc.linter)
			}
			req := &packages.DriverRequest{Mode: golangciMode, Env: os.Environ(), Tests: true, BuildFlags: golangciFlags}
			if tc.mutate != nil {
				tc.mutate(req)
			}
			resp, rep, err := serve(context.Background(), req, []string{"./..."})
			if err != nil {
				t.Fatal(err)
			}
			if !resp.NotHandled || rep.Declined == "" {
				t.Fatalf("served a request it must decline: %+v", rep)
			}
		})
	}
}

// TestGoPackagesThroughDriver loads the module through go/packages with the
// driver binary as GOPACKAGESDRIVER and type-checks the roots from source
// against the export data the driver serves for everything else.
func TestGoPackagesThroughDriver(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	archives := archivesFor(t, mod)
	bin := filepath.Join(t.TempDir(), "export-driver")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustGetwdOf(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build driver: %v\n%s", err, out)
	}
	logPath := filepath.Join(t.TempDir(), "driver.log")
	t.Chdir(mod)
	env := append(os.Environ(),
		"GOPACKAGESDRIVER="+bin,
		envArchives+"="+archives,
		envLog+"="+logPath,
		envWorkDir+"="+t.TempDir(),
	)
	pkgs, err := packages.Load(&packages.Config{
		// Without NeedDeps, go/packages type-checks only the roots from
		// source and loads every dependency from its export data.
		Mode:  golangciMode&^packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		Env:   env,
		Tests: true,
	}, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("%d errors loading through the driver", n)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("driver was not used: %v", err)
	}
	if strings.Contains(string(log), "declined") {
		t.Fatalf("driver declined: %s", log)
	}
}

var driverSourceDir string

func init() {
	if wd, err := os.Getwd(); err == nil {
		driverSourceDir = wd
	}
}

func mustGetwdOf(t *testing.T) string {
	t.Helper()
	if driverSourceDir == "" {
		t.Fatal("unknown driver source directory")
	}
	return driverSourceDir
}

// writeFiles adds files to a module.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func manifestArchive(t *testing.T, archives, pkgPath string) string {
	t.Helper()
	m, err := readManifest(archives)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m.Packages[pkgPath]
	if !ok {
		t.Fatalf("manifest has no %s", pkgPath)
	}
	return filepath.Join(archives, e.Archive)
}

func packageByID(t *testing.T, resp *packages.DriverResponse, id string) *packages.Package {
	t.Helper()
	for _, p := range resp.Packages {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no package %s", id)
	return nil
}

// TestChangedEmbedIsNotPrebuilt checks that embedded files are part of what
// an archive is verified against: they decide whether it compiles.
func TestChangedEmbedIsNotPrebuilt(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	archives := archivesFor(t, mod)
	stale := manifestArchive(t, archives, "example.com/m/app")
	writeFiles(t, mod, map[string]string{"app/data/msg.txt": "changed\n"})
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	if got := packageByID(t, resp, "example.com/m/app").ExportFile; got == stale || got == "" {
		t.Errorf("app's export data = %q, want a fresh archive after its embedded file changed", got)
	}
}

// TestCompileErrorIsReported checks that an error only the compiler finds
// reaches the package, as through go list -export: go/types accepts a
// //go:embed directive on an int, the compiler doesn't.
func TestCompileErrorIsReported(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	writeFiles(t, mod, map[string]string{
		"lib/bad.go":      "package lib\n\nimport _ \"embed\"\n\n//go:embed bad.txt\nvar bad int\n\n// Bad is bad.\nfunc Bad() int { return bad }\n",
		"lib/bad.txt":     "1\n",
		"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) { _ = Old() }\n",
	})
	archives := archivesFor(t, mod)
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	// golangci-lint lints lib through its test variant, so the variant
	// must carry the error too.
	for _, id := range []string{"example.com/m/lib", "example.com/m/lib [example.com/m/lib.test]"} {
		p := packageByID(t, resp, id)
		var found bool
		for _, e := range p.Errors {
			if strings.Contains(e.Msg, "go:embed") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the compile error was dropped: errors %v", id, p.Errors)
		}
	}
}

// TestCgoFlagsChangeArchive checks that a cgo package is verified against
// cgo's output, which depends on the C flags, not just its sources.
func TestCgoFlagsChangeArchive(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("no C compiler")
	}
	mod := writeModule(t, toolchainLang())
	writeFiles(t, mod, map[string]string{
		"cval/cval.go": "// Package cval exports a C constant.\npackage cval\n\n/*\n#ifndef VALUE\n#define VALUE 1\n#endif\n*/\nimport \"C\"\n\n// Value is C's VALUE.\nconst Value = C.VALUE\n",
		"app/cval.go":  "package app\n\nimport \"example.com/m/cval\"\n\n// CValue is cval's value.\nconst CValue = cval.Value\n",
	})
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("CGO_CFLAGS", "-DVALUE=1")
	archives := archivesFor(t, mod)
	prebuilt := manifestArchive(t, archives, "example.com/m/cval")

	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	if got := packageByID(t, resp, "example.com/m/cval").ExportFile; got != prebuilt {
		t.Errorf("with the same C flags, cval's export data = %q, want its archive %q", got, prebuilt)
	}

	t.Setenv("CGO_CFLAGS", "-DVALUE=2")
	resp, rep = serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	if got := packageByID(t, resp, "example.com/m/cval").ExportFile; got == prebuilt || got == "" {
		t.Errorf("after the C flags changed, cval's export data = %q, want a fresh archive", got)
	}
}

// writeCgoModule adds a cgo package to a module: cval exports a C constant
// and a Go constant from a raw string in an ordinary Go file.
func writeCgoModule(t *testing.T, mod, raw string) {
	t.Helper()
	writeFiles(t, mod, map[string]string{
		"cval/cval.go":  "// Package cval exports a C constant.\npackage cval\n\n/*\n#define VALUE 1\n*/\nimport \"C\"\n\n// Value is C's VALUE.\nconst Value = C.VALUE\n",
		"cval/plain.go": "package cval\n\n// Raw is a raw string.\nconst Raw = `first\n" + raw + "\n`\n",
	})
}

// TestCgoArchiveSurvivesRelocation checks that a cgo package's archive still
// verifies when the same sources are linted from another directory: cgo's
// output names that directory.
func TestCgoArchiveSurvivesRelocation(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("no C compiler")
	}
	t.Setenv("CGO_ENABLED", "1")
	mod := writeModule(t, toolchainLang())
	writeCgoModule(t, mod, "//line x.go:1")
	archives := archivesFor(t, mod)
	prebuilt := manifestArchive(t, archives, "example.com/m/cval")

	moved := filepath.Join(t.TempDir(), "elsewhere")
	if out, err := exec.Command("cp", "-a", mod, moved).CombinedOutput(); err != nil {
		t.Fatalf("copy module: %v: %s", err, out)
	}
	resp, rep := serveModule(t, moved, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	if got := packageByID(t, resp, "example.com/m/cval").ExportFile; got != prebuilt {
		t.Errorf("relocated, cval's export data = %q, want its archive %q", got, prebuilt)
	}
}

// TestCgoPlainFileContentIsHashed checks that leaving out cgo's //line
// directives never leaves out source: a raw string line that looks like
// one is still content.
func TestCgoPlainFileContentIsHashed(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("no C compiler")
	}
	t.Setenv("CGO_ENABLED", "1")
	mod := writeModule(t, toolchainLang())
	writeCgoModule(t, mod, "//line x.go:1")
	archives := archivesFor(t, mod)
	prebuilt := manifestArchive(t, archives, "example.com/m/cval")
	writeCgoModule(t, mod, "//line y.go:2")
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	if got := packageByID(t, resp, "example.com/m/cval").ExportFile; got == prebuilt || got == "" {
		t.Errorf("after cval's raw string changed, its export data = %q, want a fresh archive", got)
	}
}

// TestTestFileCompileErrorIsReported checks that an error only the compiler
// reports, in a test file, reaches the test variant golangci-lint lints.
func TestTestFileCompileErrorIsReported(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	writeFiles(t, mod, map[string]string{
		"lib/embed_test.go": "package lib\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n)\n\n//go:embed lib.go\nvar n int\n\nfunc TestN(t *testing.T) { _ = n }\n",
	})
	archives := archivesFor(t, mod)
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	v := packageByID(t, resp, "example.com/m/lib [example.com/m/lib.test]")
	var found bool
	for _, e := range v.Errors {
		if strings.Contains(e.Msg, "go:embed") {
			found = true
		}
	}
	if !found {
		t.Errorf("lib's test compile error was dropped: errors %v", v.Errors)
	}
}

// TestBodylessTestFuncIsReported checks a compile-only error without a
// directive: a function without a body in a test file.
func TestBodylessTestFuncIsReported(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	writeFiles(t, mod, map[string]string{
		"lib/body_test.go": "package lib\n\nimport \"testing\"\n\nfunc helper()\n\nfunc TestHelper(t *testing.T) { helper() }\n",
	})
	archives := archivesFor(t, mod)
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	v := packageByID(t, resp, "example.com/m/lib [example.com/m/lib.test]")
	var found bool
	for _, e := range v.Errors {
		if strings.Contains(e.Msg, "missing function body") {
			found = true
		}
	}
	if !found {
		t.Errorf("lib's test compile error was dropped: errors %v", v.Errors)
	}
}

// TestMissingArchiveIsNotUsed checks that a manifest entry whose archive is
// gone is not served.
func TestMissingArchiveIsNotUsed(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	archives := archivesFor(t, mod)
	gone := manifestArchive(t, archives, "example.com/m/lib")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	resp, rep := serveModule(t, mod, archives, nil)
	if resp.NotHandled {
		t.Fatalf("declined: %s", rep.Declined)
	}
	lib := packageByID(t, resp, "example.com/m/lib")
	if lib.ExportFile == gone || lib.ExportFile == "" {
		t.Errorf("lib's export data = %q, want a fresh archive for the missing one", lib.ExportFile)
	}
	if _, err := os.Stat(lib.ExportFile); err != nil {
		t.Errorf("lib's export data: %v", err)
	}
}

// TestTagsOnTheCommandLineOverrideGOFLAGS checks that the tags the go
// command builds with are the ones compared: a -tags flag overrides
// GOFLAGS', in the archives a builder records and in the request.
func TestTagsOnTheCommandLineOverrideGOFLAGS(t *testing.T) {
	mod := writeModule(t, toolchainLang())
	t.Setenv("GOFLAGS", "-tags=envtag")
	out := t.TempDir()
	t.Chdir(mod)
	if err := writeManifestDir(context.Background(), out, []string{"-tags=explicit", "--", "./..."}); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(m.BuildTags, []string{"explicit"}) {
		t.Errorf("manifest buildTags = %v, want [explicit]: the flag overrides GOFLAGS", m.BuildTags)
	}
	resp, rep := serveModule(t, mod, out, func(r *packages.DriverRequest) {
		r.BuildFlags = append(r.BuildFlags, "-tags=explicit")
		r.Env = os.Environ()
	})
	if resp.NotHandled {
		t.Errorf("declined archives built with the tags the request builds with: %s", rep.Declined)
	}
}

// TestQuotedTagsParseAsGoDoes checks GOFLAGS' quoting and the -tags forms
// the go command accepts: tags it builds with from GOFLAGS alone match the
// same tags set on the command line.
func TestQuotedTagsParseAsGoDoes(t *testing.T) {
	for _, tc := range []struct{ goflags, flag string }{
		{"-tags='envtag'", "-tags=envtag"},
		{"'-tags=a b'", "-tags=a,b"},
		{"\"-tags=a,b\"", "-tags=b,a"},
	} {
		t.Run(tc.goflags, func(t *testing.T) {
			mod := writeModule(t, toolchainLang())
			t.Setenv("GOFLAGS", tc.goflags)
			out := t.TempDir()
			t.Chdir(mod)
			if err := writeManifestDir(context.Background(), out, []string{"--", "./..."}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GOFLAGS", "")
			resp, rep := serveModule(t, mod, out, func(r *packages.DriverRequest) {
				r.BuildFlags = append(r.BuildFlags, tc.flag)
				r.Env = os.Environ()
			})
			if resp.NotHandled {
				t.Errorf("declined archives built with GOFLAGS=%s for a request with %s: %s", tc.goflags, tc.flag, rep.Declined)
			}
		})
	}
}

// linterIs makes the driver see a linter built with Go version v for the
// rest of the test, instead of reading its parent process's binary.
func linterIs(t *testing.T, v string) {
	t.Helper()
	old := linterGoVersion
	linterGoVersion = func() (string, error) { return v, nil }
	t.Cleanup(func() { linterGoVersion = old })
}
