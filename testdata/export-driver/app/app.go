// Package app has findings that need its dependencies' export data and
// facts: deprecations in the standard library and in a replaced module.
package app

import (
	"io/ioutil"
	"strings"

	"example.com/exportlib"
)

// Read reads everything.
func Read() (string, error) {
	data, err := ioutil.ReadAll(strings.NewReader("x"))
	if err != nil {
		return "", err
	}
	return string(data) + exportlib.Old(), nil
}

// Count counts.
func Count() int {
	n := 1
	n = 2
	n = 3
	return n
}
