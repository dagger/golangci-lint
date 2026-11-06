// write-manifest prints the manifest.json golangci-lint's export driver
// reads for a directory of archives a per-package builder compiled:
//
//	write-manifest <list> <env> <entries> <tags> <dir>
//
// list is the `go list -json` listing of the packages, with
// manifest.ListFields at least, and env the settings of the go command that
// compiled them, as `write-manifest env` prints them where it ran. tags are
// the build tags, separated by commas. Each line of entries is a package's ID
// and its archive's path, then for a cgo package the Go files cgo wrote for
// its compile, in the compile's order, separated by tabs, paths relative to
// the archives directory. dir is where those files of cgo's output are, at
// the same paths.
//
//	write-manifest env
//
// prints what `go env -json` reports for the settings a manifest records
// (manifest.EnvVars).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/dagger/golangci-lint/helpers/export-driver/manifest"
)

func main() {
	ctx := context.Background()
	var err error
	switch {
	case len(os.Args) == 2 && os.Args[1] == "env":
		cmd := exec.CommandContext(ctx, "go", append([]string{"env", "-json"}, manifest.EnvVars...)...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
	case len(os.Args) == 6:
		var env map[string]string
		env, err = readEnv(os.Args[2])
		if err == nil {
			err = run(ctx, os.Args[1], os.Args[3], os.Args[4], os.Args[5], env, os.Stdout)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: write-manifest <list> <env> <entries> <tags> <dir>, or write-manifest env")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "write-manifest:", err)
		os.Exit(1)
	}
}

// readEnv reads what `write-manifest env` printed.
func readEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return env, nil
}

func run(ctx context.Context, listPath, entriesPath, tags, dir string, env map[string]string, out io.Writer) error {
	listed := map[string]manifest.Package{}
	list, err := os.ReadFile(listPath)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(list))
	for {
		var p manifest.Package
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("%s: %w", listPath, err)
		}
		listed[p.ImportPath] = p
	}
	data, err := os.ReadFile(entriesPath)
	if err != nil {
		return err
	}
	var lines [][]string
	for _, l := range strings.Split(string(data), "\n") {
		if fields := strings.Split(l, "\t"); len(fields) >= 2 {
			lines = append(lines, fields)
		}
	}

	m := manifest.New(ctx, env, strings.FieldsFunc(tags, func(r rune) bool { return r == ',' }))
	entries := make([]manifest.Entry, len(lines))
	errs := make([]error, len(lines))
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for i, fields := range lines {
		p, ok := listed[fields[0]]
		if !ok {
			return fmt.Errorf("%s: not a package the listing lists", fields[0])
		}
		outputs := fields[2:]
		generated := make([]string, len(outputs))
		for j, o := range outputs {
			generated[j] = filepath.Join(dir, filepath.FromSlash(o))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			entries[i], errs[i] = manifest.NewEntry(p, fields[1], generated, outputs, env["GOMODCACHE"])
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for i, fields := range lines {
		m.Packages[fields[0]] = entries[i]
	}
	data, err = m.Encode()
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}
