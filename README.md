# golangci-lint

A Dagger module for linting Go workspaces with golangci-lint.

## Usage

Install the module in a workspace and run its check:

```sh
dagger install github.com/dagger/golangci-lint
dagger check golangci-lint:lint-all
```

The module discovers `go.mod` files at or below the working directory, including
the enclosing module when run from a subdirectory. Each selected Go module is
linted separately, with diagnostics prefixed by its workspace-relative path.

Configure module selection in the consuming workspace's `dagger.toml`:

```toml
[modules.golangci-lint]
source = "github.com/dagger/golangci-lint"
settings.lint = ["**", "!docs", "!examples/legacy"]
settings.includeExtraFiles = ["shared/config/**"]
```

Bare paths and `path/**` select that module and its descendants. `!` excludes,
and exclusions always win. An empty list or an exclude-only list starts with all
modules selected. `["!**"]` skips everything. Other glob forms are not supported
by the `lint` setting.

The default container uses **golangci-lint 2.11.4**, pinned by digest, and includes
a C/C++ toolchain for cgo typechecking. Set `settings.version` to choose another
golangci-lint version. Dang callers can instead supply `base: Container!` with
Go, golangci-lint, and any required system libraries installed. `version` and
`base` are mutually exclusive.

## API

| Entry point | Purpose |
| --- | --- |
| `lintAll(ws)` | Lint all selected Go modules (`@check`). |
| `modules(ws)` | Discover Go modules; optionally filter by include/exclude patterns or skip selection. |
| `module(ws, path)` | Find the Go module containing a directory. |
| `module(...).lint(ws)` | Lint one module, respecting selection. |
| `module(...).skipLint(ws)` | Inspect whether the module is excluded. |
| `module(...).include(ws)` | Inspect workspace-root source patterns. |
| `module(...).source(ws)` | Inspect the directory mounted for linting. |

Source discovery preserves Go and C/C++ sources, nested module boundaries,
`go:embed` assets, and transitive local `go.mod` replacements within the workspace.
It includes `.golangci.yml`, `.golangci.yaml`, `.golangci.toml`, and
`.golangci.json` configuration files. Use `includeExtraFiles` for additional
inputs. Test- and generate-only include directives are ignored.

## Development

```sh
dagger check -m .dagger/modules/e2e
```

The suite covers module discovery, selection, successful and failing lint runs,
configuration loading, custom containers, cgo, embedded assets, local
replacements, and the source-discovery helper's unit tests.

To run the helper tests directly with Go 1.25 or newer:

```sh
cd helpers/go-includes
go test ./...
```
