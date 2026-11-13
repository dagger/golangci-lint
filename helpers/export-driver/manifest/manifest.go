// Package manifest is the format of a directory of prebuilt per-package
// archives that golangci-lint's export driver reads: manifest.json, which
// names each package's archive and a digest of what it was compiled from,
// with the settings they were all compiled with. A per-package builder writes
// it with New and NewEntry, or with the write-manifest command; the driver
// reads it with Read, and checks each archive with SourcesDigest.
//
// It depends on the standard library alone, so that a builder can build it
// from source without downloading anything.
package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Version is the only manifest layout this package writes and reads.
const Version = 2

// Manifest describes a directory of prebuilt per-package archives: the
// output of `go tool compile -pack` for each package, export data at least,
// test variants included, with the settings they were compiled with.
// Packages are keyed by their ID as go list -test names them: the import
// path, or "p [p.test]" for a variant compiled for p's test.
type Manifest struct {
	Version    int    `json:"version"`
	GoVersion  string `json:"goVersion"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	CgoEnabled bool   `json:"cgoEnabled"`
	// Cgo, with cgo enabled, is what cgo's output depends on besides the
	// sources (CgoSettings): the archives' cgo output is good only for the
	// same.
	Cgo       map[string]string `json:"cgo,omitempty"`
	BuildTags []string          `json:"buildTags"`
	Packages  map[string]Entry  `json:"packages"`
}

// Entry is a package's archive.
type Entry struct {
	// Archive is the archive's path relative to the manifest's directory.
	Archive string `json:"archive"`
	// Sources is SourcesDigest of what the archive was compiled from.
	Sources string `json:"sources"`
	// CgoFiles, for a cgo package, are its cgo files' names, relative to
	// the package directory, and CgoOutput the Go files cgo wrote for its
	// compile, relative to the manifest's directory, in the compile's order:
	// the package compiles its other Go files and those.
	CgoFiles  []string `json:"cgoFiles,omitempty"`
	CgoOutput []string `json:"cgoOutput,omitempty"`
	// Module, for a cgo package, is the module version its files come from,
	// "path@version" (ModuleVersion), or "" for the main module's or a local
	// replacement's: what its C compiles may read beyond its digest.
	Module string `json:"module,omitempty"`
}

// EnvVars are the go command's settings New reads from `go env -json`: those
// a manifest records, and GOMODCACHE, which NewEntry needs.
var EnvVars = append([]string{"GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOMODCACHE"}, CgoVars...)

// New is the manifest of archives compiled with env, as `go env -json`
// reports EnvVars, and with tags, the build tags, with no packages yet.
func New(ctx context.Context, env map[string]string, tags []string) *Manifest {
	m := &Manifest{
		Version:    Version,
		GoVersion:  env["GOVERSION"],
		GOOS:       env["GOOS"],
		GOARCH:     env["GOARCH"],
		CgoEnabled: env["CGO_ENABLED"] == "1",
		BuildTags:  append([]string{}, tags...),
		Packages:   map[string]Entry{},
	}
	sort.Strings(m.BuildTags)
	if m.CgoEnabled {
		m.Cgo = CgoSettings(ctx, env)
	}
	return m
}

// Read reads the manifest in dir, which must be of this Version.
func Read(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version != Version {
		return nil, fmt.Errorf("manifest version %d, want %d", m.Version, Version)
	}
	return &m, nil
}

// Encode is the manifest as manifest.json holds it.
func (m *Manifest) Encode() ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ListingFile is the file in a manifest's directory, beside manifest.json,
// that a builder may write with the listing its plan made of the packages
// outside the main module: `go list -deps -json=ListingFields` output, one
// package after another. With it, a caller that type-checks the main module
// from source and loads every other package from the archives needs no go
// command, and no module cache, to know them.
const ListingFile = "listing.json"

// ListingFields are the fields of `go list -json` ListingFile holds.
const ListingFields = "ImportPath,Name,ForTest,Imports,ImportMap"

// ListFields are the fields of `go list -json` a Package holds.
const ListFields = "ImportPath,ForTest,Dir,Module,GoFiles,CgoFiles,CFiles,CXXFiles,MFiles,HFiles,FFiles,SFiles,SwigFiles,SwigCXXFiles,SysoFiles,EmbedFiles"

// Package is a package as `go list -json=ListFields` lists it, its files
// relative to Dir.
type Package struct {
	ImportPath, ForTest, Dir string
	Module                   *Module
	GoFiles, CgoFiles        []string
	// What go/packages calls OtherFiles.
	CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles []string
	SwigFiles, SwigCXXFiles, SysoFiles               []string
	EmbedFiles                                       []string
}

// Module is a package's module as `go list -json` lists it.
type Module struct {
	Path, Version string
	Replace       *Module
}

// ModuleVersion is the module version a package's files come from, the
// replacement's if any, "path@version", or "" for files no version fixes:
// the main module's, or a local replacement's.
func ModuleVersion(m *Module) string {
	if m != nil && m.Replace != nil {
		m = m.Replace
	}
	if m == nil || m.Version == "" {
		return ""
	}
	return m.Path + "@" + m.Version
}

// Files is what p's archive is compiled from, with generated, for a cgo
// package, the Go files cgo wrote for its compile, in the compile's order.
func (p Package) Files(generated []string) Files {
	abs := func(groups ...[]string) []string {
		var out []string
		for _, g := range groups {
			for _, f := range g {
				if !filepath.IsAbs(f) {
					f = filepath.Join(p.Dir, f)
				}
				out = append(out, f)
			}
		}
		return out
	}
	return Files{
		Dir:             p.Dir,
		GoFiles:         abs(p.GoFiles, p.CgoFiles),
		OtherFiles:      abs(p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.FFiles, p.SFiles, p.SwigFiles, p.SwigCXXFiles, p.SysoFiles),
		EmbedFiles:      abs(p.EmbedFiles),
		CompiledGoFiles: append(abs(p.GoFiles), generated...),
	}
}

// NewEntry is the entry of p's archive, at archive in the manifest's
// directory. A cgo package's compile reads generated, the Go files cgo wrote
// for it, in the compile's order, which the manifest's directory holds at
// outputs. modcache is GOMODCACHE: only a package from the module cache, not
// a copy such as a vendor directory's, is of its module version.
func NewEntry(p Package, archive string, generated, outputs []string, modcache string) (Entry, error) {
	if len(p.CgoFiles) > 0 && len(generated) == 0 {
		return Entry{}, fmt.Errorf("%s: a cgo package without cgo's output", p.ImportPath)
	}
	if len(generated) != len(outputs) {
		return Entry{}, fmt.Errorf("%s: %d files of cgo's output, at %d places", p.ImportPath, len(generated), len(outputs))
	}
	digest, err := SourcesDigest(p.Files(generated))
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", p.ImportPath, err)
	}
	e := Entry{Archive: filepath.ToSlash(archive), Sources: digest}
	if len(p.CgoFiles) > 0 {
		e.CgoFiles = p.CgoFiles
		for _, o := range outputs {
			e.CgoOutput = append(e.CgoOutput, filepath.ToSlash(o))
		}
		if Within(modcache, p.Dir) {
			e.Module = ModuleVersion(p.Module)
		}
	}
	return e, nil
}

// Within reports whether path is dir or below it.
func Within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return dir != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// CgoVars are the go command's settings cgo's output depends on.
var CgoVars = []string{"CC", "CXX", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_FFLAGS", "CGO_LDFLAGS"}

// CgoSettings is what cgo's output depends on besides a package's sources,
// from go env: CgoVars, and "ccVersion", the first line the C compiler
// prints for --version, or "" if it prints none.
func CgoSettings(ctx context.Context, env map[string]string) map[string]string {
	settings := map[string]string{}
	for _, v := range CgoVars {
		settings[v] = env[v]
	}
	if cc := strings.Fields(env["CC"]); len(cc) > 0 {
		out, _ := exec.CommandContext(ctx, cc[0], append(cc[1:], "--version")...).Output()
		first, _, _ := strings.Cut(string(out), "\n")
		settings["ccVersion"] = strings.TrimSpace(first)
	}
	return settings
}

// Files is what an archive's export data is compiled from, as go/packages
// or go list reports it: all paths absolute.
type Files struct {
	Dir             string
	GoFiles         []string // Go and cgo files
	OtherFiles      []string // C, C++, header, assembly, Fortran, Objective-C, SWIG and syso files
	EmbedFiles      []string
	CompiledGoFiles []string // the Go files compiled: GoFiles, or cgo's output
}

// UsesCgo reports whether the compiled Go files are cgo's output rather than
// the package's Go files.
func (f Files) UsesCgo() bool {
	if len(f.CompiledGoFiles) != len(f.GoFiles) {
		return true
	}
	gofiles := map[string]bool{}
	for _, g := range f.GoFiles {
		gofiles[g] = true
	}
	for _, c := range f.CompiledGoFiles {
		if !gofiles[c] {
			return true
		}
	}
	return false
}

// SourcesDigest identifies what a package's archive is compiled from: sha256
// over the sorted lines "<name> <sha256 of content>\n". It covers the Go
// files, the other source files and the embedded files, each named by its
// path relative to the package directory. A cgo package is compiled from
// cgo's output as well as its plain Go files, and cgo's output is what
// depends on the C toolchain, headers and flags, so it covers that output as
// well as the cgo files. The go command names cgo's output files after a
// hash of their contents, and their //line directives name the directory
// cgo ran in, so they are named by their position among the generated files
// and hashed without those directives.
func SourcesDigest(f Files) (string, error) {
	var lines []string
	add := func(name, path string, generated bool) error {
		h, err := fileSHA256(path, generated)
		if err != nil {
			return err
		}
		lines = append(lines, name+" "+h+"\n")
		return nil
	}
	rel := func(path string) string {
		if r, err := filepath.Rel(f.Dir, path); err == nil {
			return filepath.ToSlash(r)
		}
		return path
	}
	if f.UsesCgo() {
		lines = append(lines, "cgo\n")
		plain := map[string]bool{}
		for _, g := range f.GoFiles {
			plain[g] = true
		}
		compiled := map[string]bool{}
		for _, c := range f.CompiledGoFiles {
			compiled[c] = true
		}
		for _, g := range f.GoFiles {
			if !compiled[g] {
				if err := add(rel(g), g, false); err != nil {
					return "", err
				}
			}
		}
		generated := 0
		for _, c := range f.CompiledGoFiles {
			var err error
			if plain[c] {
				err = add(rel(c), c, false)
			} else {
				err = add(fmt.Sprintf("cgo-output-%d", generated), c, true)
				generated++
			}
			if err != nil {
				return "", err
			}
		}
	} else {
		for _, g := range f.GoFiles {
			if err := add(rel(g), g, false); err != nil {
				return "", err
			}
		}
	}
	for _, group := range [][]string{f.OtherFiles, f.EmbedFiles} {
		for _, o := range group {
			if err := add(rel(o), o, false); err != nil {
				return "", err
			}
		}
	}
	sort.Strings(lines)
	sum := sha256.New()
	for _, l := range lines {
		_, _ = io.WriteString(sum, l) // a hash never fails to write
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil)), nil
}

// fileSHA256 hashes a file's content. For a generated Go file, it leaves out
// the //line directives: the comments, as the Go scanner finds them, that
// start with "//line ".
func fileSHA256(path string, generated bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if !generated {
		_, _ = h.Write(data) // a hash never fails to write
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	fset := token.NewFileSet()
	file := fset.AddFile(path, -1, len(data))
	var s scanner.Scanner
	s.Init(file, data, nil, scanner.ScanComments)
	last := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && strings.HasPrefix(lit, "//line ") {
			off := file.Offset(pos)
			_, _ = h.Write(data[last:off])
			last = off + len(lit)
		}
	}
	_, _ = h.Write(data[last:])
	return hex.EncodeToString(h.Sum(nil)), nil
}
