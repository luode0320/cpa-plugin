// Package usagestats implements the merged token-usage statistics stack for
// the workbuddy plugin. It was ported from AITNR/cap-token-usage-tracker
// (MIT) and adapted from a standalone UsagePlugin into an in-process library:
// the data source is the workbuddy executor chain instead of the host's
// UsagePlugin broadcast.
package usagestats

import (
	"fmt"
	"time"
)

// statusError carries an HTTP-ish status code alongside an error, matching the
// conventions used by the ported statistics stack.
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

// withStatus wraps err with an HTTP status code for query handlers.
func withStatus(status int, format string, args ...any) error {
	return &statusError{status: status, err: fmt.Errorf(format, args...)}
}

func errorHTTPStatus(err error) int {
	var target *statusError
	if err != nil && asStatusError(err, &target) {
		return target.status
	}
	return 500
}

func asStatusError(err error, target **statusError) bool {
	for err != nil {
		if current, ok := err.(*statusError); ok {
			*target = current
			return true
		}
		type unwrapper interface{ Unwrap() error }
		unwrapped, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// nowUTC is the clock used by the statistics stack. Kept as a var so tests can
// override it, matching the original plugin.
var nowUTC = func() time.Time { return time.Now().UTC() }

// version identifies this statistics stack in outbound requests (models.dev
// User-Agent). The main workbuddy plugin keeps its own version separately.
var version = "merged"
