// Package exportlib is a dependency loaded from export data.
package exportlib

// Old returns a word.
//
// Deprecated: use New.
func Old() string { return "old" }

// New returns a word.
func New() string { return "new" }
