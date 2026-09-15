// Discover golangci-lint inputs from a local workspace snapshot.
package main

import (
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/mod/modfile"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cliArgs []string) error {
	flags := flag.NewFlagSet("go-includes", flag.ContinueOnError)
	root := flags.String("root", ".", "workspace root to scan")
	outputDir := flags.String("output-dir", "", "directory to write one file of include patterns per module to")
	if err := flags.Parse(cliArgs); err != nil {
		return err
	}
	if *outputDir == "" {
		return fmt.Errorf("--output-dir is required")
	}

	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	index, err := indexLocal(*root)
	if err != nil {
		return err
	}
	return index.writeAllDir(*outputDir)
}

func moduleIncludeFile(moduleRoot string) string {
	name := moduleRoot
	if moduleRoot == "." {
		name = "_root_"
	}
	return filepath.FromSlash(name) + ".inc"
}

func (index *localIndex) writeAllDir(dir string) error {
	for _, moduleRoot := range index.moduleRoots {
		includes, err := index.includesFor(moduleRoot)
		if err != nil {
			return err
		}
		outPath := filepath.Join(dir, moduleIncludeFile(moduleRoot))
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		data := strings.Join(includes, "\n") + "\n"
		if err := os.WriteFile(outPath, []byte(data), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type localIndex struct {
	root            string
	moduleRoots     []string
	moduleSet       map[string]bool
	goFilesByModule map[string][]string
}

func indexLocal(root string) (*localIndex, error) {
	index := &localIndex{
		root:            root,
		moduleSet:       map[string]bool{},
		goFilesByModule: map[string][]string{},
	}
	var goMods, goFiles []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Name() == "go.mod":
			goMods = append(goMods, rel)
		case strings.HasSuffix(d.Name(), ".go"):
			goFiles = append(goFiles, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(goMods)
	for _, goModPath := range goMods {
		moduleRoot := strings.TrimSuffix(goModPath, "/go.mod")
		if goModPath == "go.mod" {
			moduleRoot = "."
		}
		index.moduleRoots = append(index.moduleRoots, moduleRoot)
		index.moduleSet[moduleRoot] = true
	}

	for _, goFile := range goFiles {
		moduleRoot, ok := containingModuleDir(index.moduleSet, path.Dir(goFile))
		if !ok {
			continue
		}
		index.goFilesByModule[moduleRoot] = append(index.goFilesByModule[moduleRoot], goFile)
	}
	return index, nil
}

func (index *localIndex) includesFor(moduleRoot string) ([]string, error) {
	queued := map[string]bool{moduleRoot: true}
	queue := []string{moduleRoot}
	var includes []string

	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]

		directives, err := index.directives(module)
		if err != nil {
			return nil, err
		}

		includes = append(includes, includeBasePatterns(module)...)
		for _, directive := range directives {
			if !directive.isEmbed() {
				continue
			}
			patterns, err := directive.includePatterns()
			if err != nil {
				return nil, err
			}
			includes = append(includes, patterns...)
		}

		next, err := index.replaceModules(module)
		if err != nil {
			return nil, err
		}
		for _, nextModule := range next {
			if !queued[nextModule] {
				queued[nextModule] = true
				queue = append(queue, nextModule)
			}
		}
	}

	deduped := make([]string, 0, len(includes))
	seen := map[string]bool{}
	for _, include := range includes {
		if seen[include] {
			continue
		}
		seen[include] = true
		deduped = append(deduped, include)
	}
	return deduped, nil
}

func (index *localIndex) directives(moduleRoot string) ([]goDirective, error) {
	files := index.goFilesByModule[moduleRoot]
	sort.Strings(files)

	var directives []goDirective
	for _, filePath := range files {
		data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(filePath)))
		if err != nil {
			return nil, err
		}
		fileDirectives, err := goDirectivesInFile(filePath, data)
		if err != nil {
			return nil, err
		}
		directives = append(directives, fileDirectives...)
	}
	return directives, nil
}

func (index *localIndex) replaceModules(moduleRoot string) ([]string, error) {
	goModPath := path.Join(moduleRoot, "go.mod")
	data, err := os.ReadFile(filepath.Join(index.root, filepath.FromSlash(goModPath)))
	if err != nil {
		return nil, err
	}
	goMod, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, err
	}

	var roots []string
	for _, replace := range goMod.Replace {
		if !isLocalReplace(replace) {
			continue
		}
		target := strings.TrimSuffix(replace.New.Path, "/")
		root := path.Join(path.Dir(goModPath), target)
		if !index.moduleSet[root] {
			return nil, fmt.Errorf("no Go module found for local replace target: %s", replace.New.Path)
		}
		roots = append(roots, root)
	}
	return roots, nil
}

func containingModuleDir(moduleSet map[string]bool, dir string) (string, bool) {
	dir = path.Clean(strings.TrimPrefix(dir, "/"))
	for {
		if moduleSet[dir] {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

func includeBasePatterns(moduleRoot string) []string {
	patterns := []string{
		"**/*.go",
		"**/*.c",
		"**/*.cc",
		"**/*.cpp",
		"**/*.cxx",
		"**/*.h",
		"**/*.hh",
		"**/*.hpp",
		"**/*.hxx",
		"**/*.s",
		"**/*.S",
		"**/*.syso",
		"go.mod",
		// FIXME: exclude nested module trees instead of uploading their Go
		// files just to preserve their module boundaries.
		"**/go.mod",
		"go.sum",
		"**/go.sum",
		"go.work",
		"go.work.sum",
	}
	for i, pattern := range patterns {
		patterns[i] = path.Join(moduleRoot, pattern)
	}
	return patterns
}

func isLocalReplace(replace *modfile.Replace) bool {
	return replace.New.Version == "" && modfile.IsDirectoryPath(replace.New.Path)
}

func goDirectivesInFile(filePath string, data []byte) ([]goDirective, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var directives []goDirective
	for _, group := range file.Comments {
		for _, comment := range group.List {
			directive := goDirective{
				filePath: filePath,
				position: fset.Position(comment.Slash).String(),
				comment:  comment.Text,
			}
			if directive.isEmbed() {
				directives = append(directives, directive)
			}
		}
	}
	return directives, nil
}

type goDirective struct {
	filePath string
	position string
	comment  string
}

func (d goDirective) dir() string {
	dir := path.Dir(d.filePath)
	if dir == "." {
		return ""
	}
	return dir
}

func (d goDirective) isEmbed() bool {
	return d.hasName("go:embed")
}

func (d goDirective) args() ([]string, error) {
	name, argString, ok := d.line()
	if !ok {
		return nil, nil
	}

	var args []string
	for argString = strings.TrimLeftFunc(argString, unicode.IsSpace); argString != ""; argString = strings.TrimLeftFunc(argString, unicode.IsSpace) {
		switch argString[0] {
		case '`', '"':
			quoted, err := strconv.QuotedPrefix(argString)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid quoted string in //%s: %s", d.position, name, argString)
			}
			arg, err := strconv.Unquote(quoted)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid quoted string in //%s: %s", d.position, name, quoted)
			}
			args = append(args, arg)
			argString = argString[len(quoted):]
			if argString != "" && strings.TrimLeftFunc(argString, unicode.IsSpace) == argString {
				return nil, fmt.Errorf("%s: invalid quoted string in //%s: %s", d.position, name, argString)
			}
		default:
			i := strings.IndexFunc(argString, unicode.IsSpace)
			if i < 0 {
				i = len(argString)
			}
			args = append(args, argString[:i])
			argString = argString[i:]
		}
	}
	return args, nil
}

func (d goDirective) includePatterns() ([]string, error) {
	if d.isEmbed() {
		patterns, err := d.args()
		if err != nil {
			return nil, err
		}
		for i, pattern := range patterns {
			patterns[i] = strings.TrimPrefix(pattern, "all:")
		}
		return d.prefixed(patterns), nil
	}
	return nil, nil
}

func (d goDirective) prefixed(patterns []string) []string {
	for i, pattern := range patterns {
		if strings.HasPrefix(pattern, "/") {
			patterns[i] = strings.TrimPrefix(pattern, "/")
			continue
		}
		patterns[i] = path.Join(d.dir(), pattern)
	}
	return patterns
}

func (d goDirective) hasName(name string) bool {
	directiveName, _, ok := d.line()
	return ok && directiveName == name
}

func (d goDirective) line() (string, string, bool) {
	if !strings.HasPrefix(d.comment, "//") {
		return "", "", false
	}
	line := strings.TrimPrefix(d.comment, "//")
	nameEnd := strings.IndexFunc(line, unicode.IsSpace)
	if nameEnd < 0 {
		nameEnd = len(line)
	}
	name := line[:nameEnd]
	if name != "go:embed" {
		return "", "", false
	}
	return name, line[nameEnd:], true
}
