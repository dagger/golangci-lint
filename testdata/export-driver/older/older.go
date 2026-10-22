// Package older declares an older Go than the linter is built with, so the
// export driver declines it; govet's stdversion must still see that.
package older

import "errors"

type codeError struct{}

func (codeError) Error() string { return "code" }

// Code reports whether err is a codeError, with an API newer than go 1.25.
func Code(err error) bool {
	_, ok := errors.AsType[codeError](err)
	return ok
}
