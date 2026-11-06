package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dagger/golangci-lint/helpers/export-driver/manifest"
	"golang.org/x/tools/go/packages"
)

// filesOf is what p is compiled from, as the manifest's digest covers it.
func filesOf(p *packages.Package) manifest.Files {
	return manifest.Files{Dir: p.Dir, GoFiles: p.GoFiles, OtherFiles: p.OtherFiles, EmbedFiles: p.EmbedFiles, CompiledGoFiles: p.CompiledGoFiles}
}

// moduleVersion is manifest.ModuleVersion of a module as go/packages
// reports it.
func moduleVersion(m *packages.Module) string {
	if m == nil {
		return ""
	}
	mod := &manifest.Module{Path: m.Path, Version: m.Version}
	if m.Replace != nil {
		mod.Replace = &manifest.Module{Path: m.Replace.Path, Version: m.Replace.Version}
	}
	return manifest.ModuleVersion(mod)
}

// buildTags returns the -tags value in build flags, sorted.
func buildTags(flags []string) []string {
	var tags []string
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		var v string
		switch {
		case f == "-tags" || f == "--tags":
			if i+1 < len(flags) {
				v = flags[i+1]
				i++
			}
		case strings.HasPrefix(f, "-tags="):
			v = strings.TrimPrefix(f, "-tags=")
		case strings.HasPrefix(f, "--tags="):
			v = strings.TrimPrefix(f, "--tags=")
		default:
			continue
		}
		tags = tagsValue(v)
	}
	sort.Strings(tags)
	return tags
}

// tagsValue parses a -tags value as the go command does: split on commas,
// unless it has a space or a quote, which the go command allows for
// compatibility, as a quoted, space-separated list.
func tagsValue(v string) []string {
	if strings.ContainsAny(v, " '") {
		tags, err := quotedSplit(v)
		if err != nil {
			return []string{v} // go rejects it; no archives match
		}
		return tags
	}
	var tags []string
	for _, t := range strings.Split(v, ",") {
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// quotedSplit splits s into fields as the go command splits GOFLAGS:
// separated by spaces, each optionally in single or double quotes, with
// nothing unescaped inside them.
func quotedSplit(s string) ([]string, error) {
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
	var fields []string
	for len(s) > 0 {
		for len(s) > 0 && isSpace(s[0]) {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		if s[0] == '"' || s[0] == '\'' {
			quote := s[0]
			s = s[1:]
			i := strings.IndexByte(s, quote)
			if i < 0 {
				return nil, fmt.Errorf("unterminated %c string", quote)
			}
			fields = append(fields, s[:i])
			s = s[i+1:]
			continue
		}
		i := 0
		for i < len(s) && !isSpace(s[i]) {
			i++
		}
		fields = append(fields, s[:i])
		s = s[i:]
	}
	return fields, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
