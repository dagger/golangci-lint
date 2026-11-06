package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dagger/golangci-lint/helpers/export-driver/manifest"
)

// writeManifestDir writes an archives directory for the packages the
// patterns need, with their tests, from `go list -export`: the layout a
// per-package builder provides, for tests and comparisons. It has every
// package go list compiles, test variants included, but the test mains, and
// a copy of cgo's output for each cgo package.
func writeManifestDir(ctx context.Context, out string, args []string) error {
	var flags, patterns []string
	for i, a := range args {
		if a == "--" {
			flags, patterns = args[:i], args[i+1:]
			break
		}
	}
	if patterns == nil {
		return fmt.Errorf("usage: export-driver --write-manifest=DIR [build flags] -- patterns")
	}
	env, err := goEnv(ctx, os.Environ())
	if err != nil {
		return err
	}
	goflags, err := quotedSplit(env["GOFLAGS"])
	if err != nil {
		return fmt.Errorf("GOFLAGS: %w", err)
	}
	listArgs := append([]string{"list", "-e", "-export", "-compiled", "-deps", "-test",
		"-json=" + manifest.ListFields + ",Export,CompiledGoFiles"},
		flags...)
	listArgs = append(listArgs, "--")
	listArgs = append(listArgs, patterns...)
	cmd := exec.CommandContext(ctx, "go", listArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go list: %w: %s", err, stderr.String())
	}
	if err := os.MkdirAll(filepath.Join(out, "archives"), 0o755); err != nil {
		return err
	}
	// Flags on the command line override GOFLAGS', so they come last.
	m := manifest.New(ctx, env, buildTags(append(goflags, flags...)))
	dec := json.NewDecoder(&stdout)
	for {
		var p struct {
			manifest.Package
			Export          string
			CompiledGoFiles []string
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if p.Export == "" || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		name := sha256.Sum256([]byte(p.ImportPath))
		path, _, _ := strings.Cut(p.ImportPath, " [")
		rel := filepath.Join("archives", hex.EncodeToString(name[:8])+"-"+filepath.Base(path)+".a")
		if err := copyFile(p.Export, filepath.Join(out, rel)); err != nil {
			return err
		}
		// cgo's output, which go list's compiled files are besides the plain
		// Go files, copied beside the archives.
		var generated, outputs []string
		if len(p.CgoFiles) > 0 {
			plain := map[string]bool{}
			for _, g := range p.GoFiles {
				plain[filepath.Join(p.Dir, g)] = true
			}
			cgoDir := filepath.Join("cgo", hex.EncodeToString(name[:8]))
			if err := os.MkdirAll(filepath.Join(out, cgoDir), 0o755); err != nil {
				return err
			}
			for _, c := range p.CompiledGoFiles {
				if !filepath.IsAbs(c) {
					c = filepath.Join(p.Dir, c)
				}
				if plain[c] {
					continue
				}
				o := filepath.Join(cgoDir, filepath.Base(c))
				if err := copyFile(c, filepath.Join(out, o)); err != nil {
					return err
				}
				generated, outputs = append(generated, c), append(outputs, o)
			}
		}
		entry, err := manifest.NewEntry(p.Package, rel, generated, outputs, env["GOMODCACHE"])
		if err != nil {
			return err
		}
		m.Packages[p.ImportPath] = entry
	}
	data, err := m.Encode()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "manifest.json"), data, 0o644)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
