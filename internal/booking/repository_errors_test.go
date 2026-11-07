// repository_errors_test.go unit-tests dbError, the shared helper that turns
// database timeouts into ErrBusy. It needs no database: it only checks how
// errors are wrapped.
package booking

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDBError_TimeoutBecomesErrBusy(t *testing.T) {
	// This is what a timed-out query or pool acquire looks like: an error
	// that wraps context.DeadlineExceeded.
	timeout := fmt.Errorf("failed to acquire connection: %w", context.DeadlineExceeded)

	err := dbError(timeout, "get show %d", 1)

	assert.ErrorIs(t, err, ErrBusy, "timeouts must map to ErrBusy (503)")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the original error must still be inside, for the logs")
	assert.Contains(t, err.Error(), "get show 1")
}

func TestDBError_OtherErrorsAreNotBusy(t *testing.T) {
	err := dbError(errors.New("syntax error at or near SELEC"), "list shows")

	assert.NotErrorIs(t, err, ErrBusy, "a real bug must stay a 500, not be disguised as overload")
	assert.Contains(t, err.Error(), "list shows: syntax error")
}
