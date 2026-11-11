package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dagger/golangci-lint/helpers/export-driver/manifest"
	"golang.org/x/tools/go/packages"
)

// listed is a package as the builder's listing (manifest.ListingFile) lists it.
type listed struct {
	ImportPath string
	Name       string
	ForTest    string
	Imports    []string
	ImportMap  map[string]string
}

// readListing reads the listing in the archives directory dir: its packages
// by import path, without test variants. ok is false when the builder wrote
// none.
func readListing(dir string) (pkgs map[string]*listed, ok bool, err error) {
	f, err := os.Open(filepath.Join(dir, manifest.ListingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	pkgs = map[string]*listed{}
	dec := json.NewDecoder(f)
	for {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, false, fmt.Errorf("%s: %w", manifest.ListingFile, err)
		}
		if p.ForTest == "" {
			pkgs[p.ImportPath] = &p
		}
	}
	return pkgs, true, nil
}

// serveFromListing answers a caller that reads declarations only, without
// tests, from the builder's listing and the archives, with no go command:
// every package outside the main module from the listing, with its archive
// as its export data, and the main module's packages from their files as
// they are on disk, which the caller type-checks from source. The main
// module is the one whose go.mod is nearest the working directory. The
// archives' packages and the listing come from one plan, so they agree; the
// main module's are read afresh, so they are never stale.
func serveFromListing(m *manifest.Manifest, dir string, listing map[string]*listed, env map[string]string, tags []string, patterns []string) (*packages.DriverResponse, report, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, report{}, err
	}
	modDir, modPath, modGo, err := mainModule(cwd)
	if err != nil {
		return declined(err.Error())
	}
	ctxt := build.Default
	ctxt.GOOS, ctxt.GOARCH = env["GOOS"], env["GOARCH"]
	ctxt.CgoEnabled = env["CGO_ENABLED"] == "1"
	ctxt.BuildTags = tags
	ctxt.ReleaseTags = nil
	for i := 1; i <= goMinor(env["GOVERSION"]); i++ {
		ctxt.ReleaseTags = append(ctxt.ReleaseTags, "go1."+strconv.Itoa(i))
	}

	rep := report{}
	byID := map[string]*packages.Package{}
	var missing []string
	var load func(ip string) (*packages.Package, error)
	load = func(ip string) (*packages.Package, error) {
		if p, ok := byID[ip]; ok {
			return p, nil
		}
		p := &packages.Package{ID: ip, PkgPath: ip, Imports: map[string]*packages.Package{}}
		byID[ip] = p
		if ip == "unsafe" {
			p.Name = "unsafe"
			return p, nil
		}
		var imports []string
		// A package the listing has is another module's, even under the main
		// module's path, such as a separately versioned submodule's.
		_, listed := listing[ip]
		if rel, own := inModule(ip, modPath); own && !listed {
			pkgDir := filepath.Join(modDir, filepath.FromSlash(rel))
			if nested, err := nestedModule(modDir, pkgDir); err != nil {
				return nil, err
			} else if nested != "" {
				return nil, fmt.Errorf("package %s is in the module at %s, not the main module", ip, nested)
			}
			bp, err := ctxt.ImportDir(pkgDir, 0)
			if err != nil {
				return nil, fmt.Errorf("package %s: %w", ip, err)
			}
			if len(bp.CgoFiles) > 0 {
				return nil, fmt.Errorf("package %s uses cgo", ip)
			}
			p.Name = bp.Name
			for _, f := range bp.GoFiles {
				p.GoFiles = append(p.GoFiles, filepath.Join(bp.Dir, f))
			}
			p.CompiledGoFiles = p.GoFiles
			imports = bp.Imports
		} else {
			l, ok := listing[ip]
			e, archived := m.Packages[ip]
			if !ok || !archived || e.Archive == "" {
				missing = append(missing, ip)
				return p, nil
			}
			p.Name = l.Name
			p.ExportFile = absArchive(dir, e.Archive)
			rep.Prebuilt++
			for _, imp := range l.Imports {
				if mapped, ok := l.ImportMap[imp]; ok {
					imp = mapped
				}
				imports = append(imports, imp)
			}
		}
		for _, imp := range imports {
			if imp == "C" {
				continue
			}
			ipkg, err := load(imp)
			if err != nil {
				return nil, err
			}
			p.Imports[imp] = ipkg
		}
		return p, nil
	}

	resp := &packages.DriverResponse{Compiler: "gc", Arch: env["GOARCH"], GoVersion: goMinor(env["GOVERSION"])}
	for _, pattern := range patterns {
		ip, err := patternPackage(cwd, modDir, modPath, pattern)
		if err != nil {
			return declined(err.Error())
		}
		root, err := load(ip)
		if err != nil {
			return declined(err.Error())
		}
		root.Module = &packages.Module{Path: modPath, Main: true, Dir: modDir, GoMod: filepath.Join(modDir, "go.mod"), GoVersion: modGo}
		resp.Roots = append(resp.Roots, root.ID)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		rep := report{Declined: "the listing or the archives lack packages the roots import", Missing: missing}
		return &packages.DriverResponse{NotHandled: true}, rep, nil
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		resp.Packages = append(resp.Packages, byID[id])
	}
	rep.Roots = len(resp.Roots)
	rep.FromListing = true
	return resp, rep, nil
}

// mainModule is the module of the go.mod nearest cwd, at or above it: its
// directory, its path and its go directive's version.
func mainModule(cwd string) (dir, modPath, goVersion string, err error) {
	for dir := cwd; ; dir = filepath.Dir(dir) {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			modPath, goVersion := goModDirectives(data)
			if modPath == "" {
				return "", "", "", fmt.Errorf("%s declares no module", filepath.Join(dir, "go.mod"))
			}
			return dir, modPath, goVersion, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", "", err
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "", "", "", fmt.Errorf("no go.mod at or above %s", cwd)
		}
	}
}

// goModDirectives is the module path and the go version a go.mod declares.
func goModDirectives(goMod []byte) (modPath, goVersion string) {
	for _, line := range strings.Split(string(goMod), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, err := strconv.Unquote(f[1])
		if err != nil {
			v = f[1]
		}
		switch f[0] {
		case "module":
			modPath = v
		case "go":
			goVersion = v
		}
	}
	return modPath, goVersion
}

// inModule reports whether import path ip is in the module modPath, and its
// directory relative to the module's.
func inModule(ip, modPath string) (string, bool) {
	if ip == modPath {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(ip, modPath+"/"); ok {
		return rest, true
	}
	return "", false
}

// patternPackage is the import path of the package a directory pattern,
// relative to cwd, names in the main module. Other patterns are declined.
func patternPackage(cwd, modDir, modPath, pattern string) (string, error) {
	if strings.Contains(pattern, "...") || !(pattern == "." || strings.HasPrefix(pattern, "./") || strings.HasPrefix(pattern, "../")) {
		return "", fmt.Errorf("pattern %q is not a directory", pattern)
	}
	rel, err := filepath.Rel(modDir, filepath.Join(cwd, pattern))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("pattern %q is outside the main module", pattern)
	}
	if rel == "." {
		return modPath, nil
	}
	return path.Join(modPath, filepath.ToSlash(rel)), nil
}

// nestedModule is the directory of a go.mod between the main module's
// directory modDir and pkgDir, pkgDir included, or "" when there is none.
func nestedModule(modDir, pkgDir string) (string, error) {
	for dir := pkgDir; dir != modDir && strings.HasPrefix(dir, modDir+string(filepath.Separator)); dir = filepath.Dir(dir) {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}
