package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Environment variables the driver reads.
const (
	// envArchives is the directory holding manifest.json and the archives.
	// Unset or empty, the driver declines every request.
	envArchives = "GOLANGCI_EXPORT_ARCHIVES"
	// envLog, if set, is a file the driver appends one JSON line per
	// request to, describing what it decided.
	envLog = "GOLANGCI_EXPORT_DRIVER_LOG"
	// envWorkDir is where compiled archives are written; they must outlive
	// the driver, since the caller reads them after it exits.
	envWorkDir = "GOLANGCI_EXPORT_WORKDIR"
)

// report is the JSON line written to envLog for each request.
type report struct {
	Declined string   `json:"declined,omitempty"`
	Roots    int      `json:"roots,omitempty"`
	Prebuilt int      `json:"prebuilt,omitempty"`
	Compiled int      `json:"compiled,omitempty"`
	GoList   int      `json:"golist,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	// TestsCompiled counts the root test packages compiled to check for
	// errors only the compiler reports.
	TestsCompiled int `json:"testsCompiled,omitempty"`
	// CompileErrors are packages this driver failed to compile, which
	// fell back to the go command.
	CompileErrors []string `json:"compileErrors,omitempty"`
}

func runDriver(ctx context.Context, patterns []string, stdin io.Reader, stdout io.Writer) error {
	var req packages.DriverRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return fmt.Errorf("decode driver request: %w", err)
	}
	resp, rep, err := serve(ctx, &req, patterns)
	if err != nil {
		return err
	}
	if err := writeReport(rep); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(resp)
}

func writeReport(rep report) error {
	path := os.Getenv(envLog)
	if path == "" {
		return nil
	}
	line, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func declined(reason string) (*packages.DriverResponse, report, error) {
	return &packages.DriverResponse{NotHandled: true}, report{Declined: reason}, nil
}

// serve answers a request from go/packages. It declines (NotHandled, so the
// caller falls back to its own go list driver) whenever the prebuilt
// archives may not match what the go command would build, or the caller
// would type-check differently than without the driver.
func serve(ctx context.Context, req *packages.DriverRequest, patterns []string) (*packages.DriverResponse, report, error) {
	dir := os.Getenv(envArchives)
	if dir == "" {
		return declined("no archives configured")
	}
	if len(req.Overlay) > 0 {
		return declined("request has overlays")
	}
	if reason := unsupportedBuildFlags(req.BuildFlags); reason != "" {
		return declined(reason)
	}
	m, err := readManifest(dir)
	if err != nil {
		return declined("manifest: " + err.Error())
	}
	env, err := goEnv(ctx, req.Env)
	if err != nil {
		return nil, report{}, err
	}
	goflags, err := quotedSplit(env["GOFLAGS"])
	if err != nil {
		return declined("GOFLAGS: " + err.Error())
	}
	if reason := unsupportedGOFLAGS(goflags); reason != "" {
		return declined(reason)
	}
	switch {
	case m.GoVersion != env["GOVERSION"]:
		return declined(fmt.Sprintf("archives built by %s, go command is %s", m.GoVersion, env["GOVERSION"]))
	case m.GOOS != env["GOOS"] || m.GOARCH != env["GOARCH"]:
		return declined(fmt.Sprintf("archives built for %s/%s, not %s/%s", m.GOOS, m.GOARCH, env["GOOS"], env["GOARCH"]))
	case m.CgoEnabled != (env["CGO_ENABLED"] == "1"):
		return declined("archives built with a different CGO_ENABLED")
	}
	// Flags on the command line override GOFLAGS', so they come last.
	tags := buildTags(append(goflags, req.BuildFlags...))
	if !sameStrings(tags, nonNil(m.BuildTags)) {
		return declined(fmt.Sprintf("archives built with tags %v, request has %v", m.BuildTags, tags))
	}
	linterGo, err := linterGoVersion()
	if err != nil {
		return declined("linter Go version: " + err.Error())
	}

	mode := req.Mode&^packages.NeedExportFile |
		packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
		packages.NeedImports | packages.NeedDeps | packages.NeedModule | packages.NeedEmbedFiles | packages.NeedEmbedPatterns
	cfg := &packages.Config{
		Context:    ctx,
		Mode:       mode,
		Env:        append(slices.Clone(req.Env), "GOPACKAGESDRIVER=off"),
		BuildFlags: req.BuildFlags,
		Tests:      req.Tests,
	}
	roots, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, report{}, err
	}

	// Through a driver the caller loses Module, so it type-checks with the
	// Go version it was built with instead of the module's. Only serve
	// modules whose language version that is.
	for _, root := range roots {
		if root.Module == nil || root.Module.GoVersion == "" {
			return declined("package " + root.ID + " has no module Go version")
		}
		if langVersion(root.Module.GoVersion) != langVersion(linterGo) {
			return declined(fmt.Sprintf("module %s is go %s, linter built with %s", root.Module.Path, root.Module.GoVersion, linterGo))
		}
	}

	rep := report{Roots: len(roots)}
	if req.Mode&packages.NeedExportFile != 0 {
		if err := assignExportData(ctx, cfg, m, dir, roots, &rep); err != nil {
			return nil, report{}, err
		}
	}

	var all []*packages.Package
	packages.Visit(roots, nil, func(p *packages.Package) { all = append(all, p) })
	resp := &packages.DriverResponse{
		Compiler:  "gc",
		Arch:      env["GOARCH"],
		Packages:  all,
		GoVersion: goMinor(env["GOVERSION"]),
	}
	for _, r := range roots {
		resp.Roots = append(resp.Roots, r.ID)
	}
	return resp, rep, nil
}

// unsupportedBuildFlags rejects build flags that change what gets compiled
// in ways the manifest doesn't record. golangci-lint passes only -tags, -mod
// and -buildvcs, which stamps binaries only.
func unsupportedBuildFlags(flags []string) string {
	for i := 0; i < len(flags); i++ {
		f := strings.TrimPrefix(flags[i], "-")
		f = strings.TrimPrefix(f, "-")
		switch {
		case f == "tags":
			i++
		case strings.HasPrefix(f, "tags="), strings.HasPrefix(f, "mod="), f == "modcacherw",
			f == "buildvcs", strings.HasPrefix(f, "buildvcs="):
		default:
			return "unsupported build flag " + flags[i]
		}
	}
	return ""
}

func unsupportedGOFLAGS(goflags []string) string {
	if reason := unsupportedBuildFlags(goflags); reason != "" {
		return "GOFLAGS: " + reason
	}
	return ""
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func goEnv(ctx context.Context, env []string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS")
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go env: %w: %s", err, stderr.String())
	}
	vals := map[string]string{}
	if err := json.Unmarshal(out, &vals); err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	return vals, nil
}

// linterGoVersion is the Go version the calling linter (the driver's parent
// process) was built with: the version it type-checks with when a package
// has no Module, as packages served by a driver don't. Tests replace it.
var linterGoVersion = parentGoVersion

func parentGoVersion() (string, error) {
	info, err := buildinfo.ReadFile(fmt.Sprintf("/proc/%d/exe", os.Getppid()))
	if err != nil {
		return "", err
	}
	return info.GoVersion, nil
}

// langVersion reduces a Go version ("go1.26.3", "1.26.6", "1.22") to its
// language version ("1.26").
func langVersion(v string) string {
	v = strings.TrimPrefix(v, "go")
	if i := strings.IndexAny(v, " -+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	minor := parts[1]
	for i, r := range minor {
		if r < '0' || r > '9' {
			minor = minor[:i]
			break
		}
	}
	return parts[0] + "." + minor
}

func goMinor(v string) int {
	parts := strings.Split(langVersion(v), ".")
	if len(parts) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(parts[1])
	return n
}

func workDir() (string, error) {
	if d := os.Getenv(envWorkDir); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
		return os.MkdirTemp(d, "export-")
	}
	return os.MkdirTemp("", "golangci-export-")
}

func absArchive(dir, rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(dir, rel)
}
