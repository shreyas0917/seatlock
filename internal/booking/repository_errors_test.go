// repository_errors_test.go unit-tests dbError, the shared helper that turns
// database timeouts and a briefly unavailable Postgres into ErrBusy. It
// needs no database: it only checks how errors are wrapped.
package booking

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// A Postgres that is briefly UNAVAILABLE (chaos/postgres-restart.sh: every
// error kind below was seen in the api logs during a restart under load) is
// "busy, retry" (503), not "broken" (500).
func TestDBError_UnavailablePostgresBecomesErrBusy(t *testing.T) {
	// A REAL connect error: nothing listens on port 1.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, connectErr := pgconn.Connect(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2")
	require.Error(t, connectErr)

	cases := map[string]error{
		"connection refused while connecting": connectErr,
		"server shutting down (57P01)": &pgconn.PgError{Severity: "FATAL", Code: "57P01",
			Message: "terminating connection due to administrator command"},
		"not accepting connections yet (57P03)": &pgconn.PgError{Severity: "FATAL", Code: "57P03",
			Message: "the database system is starting up"},
		"connection exception (08006)":    &pgconn.PgError{Code: "08006", Message: "connection failure"},
		"too many connections (53300)":    &pgconn.PgError{Code: "53300", Message: "sorry, too many clients already"},
		"connection reset in mid-query":   fmt.Errorf("read: %w", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}),
		"timeout (still busy, as before)": fmt.Errorf("acquire: %w", context.DeadlineExceeded),
	}
	for name, cause := range cases {
		err := dbError(cause, "get show %d", 1)
		assert.ErrorIs(t, err, ErrBusy, name)
		assert.ErrorIs(t, err, cause, "%s: the original error stays inside, for the logs", name)
		assert.True(t, IsDBUnavailable(cause), name)
	}
}

func TestDBError_RealBugsAreNotBusy(t *testing.T) {
	for name, cause := range map[string]error{
		"syntax error (42601)":          &pgconn.PgError{Code: "42601", Message: "syntax error at or near \"SELEC\""},
		"unknown column (42703)":        &pgconn.PgError{Code: "42703", Message: "column \"nope\" does not exist"},
		"unexpected unique violation":   &pgconn.PgError{Code: "23505", Message: "duplicate key value"},
		"plain error from our own code": errors.New("cannot scan NULL into int64"),
	} {
		assert.NotErrorIs(t, dbError(cause, "list shows"), ErrBusy, name)
		assert.False(t, IsDBUnavailable(cause), name)
	}
	assert.False(t, IsDBUnavailable(nil))
}

func TestDBError_OtherErrorsAreNotBusy(t *testing.T) {
	err := dbError(errors.New("syntax error at or near SELEC"), "list shows")

	assert.NotErrorIs(t, err, ErrBusy, "a real bug must stay a 500, not be disguised as overload")
	assert.Contains(t, err.Error(), "list shows: syntax error")
}
