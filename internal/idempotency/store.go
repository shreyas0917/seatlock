// Package idempotency makes POST /v1/bookings safe to send more than once.
//
// Where it fits:
//
//	client --Idempotency-Key: k1--> handler --Begin(k1)--> [run confirm] --Complete(k1, response)
//	client --Idempotency-Key: k1--> handler --Begin(k1)--> stored response, replayed byte for byte
//
// WHAT IDEMPOTENCY MEANS: doing something twice has the same effect as doing
// it once. A user double-clicks "Pay", or their phone retries after a
// timeout: both requests carry the SAME Idempotency-Key, so the second gets
// the first one's stored answer instead of starting a second payment.
//
// The keys live in POSTGRESQL, not Redis: confirm must keep working while
// Redis is down, and a key store that disappears with Redis would mean
// either refusing every confirm or confirming without duplicate protection.
//
// It stores and replays HTTP responses, so it sits at the HTTP edge; the
// booking service below it knows nothing about it.
package idempotency

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shreyas0917/seatlock/internal/booking"
)

// MaxKeyLength is the longest Idempotency-Key accepted (matches the CHECK
// constraint in db/migrations/0003_payments.up.sql).
const MaxKeyLength = 64

var (
	// ErrMissingKey: the Idempotency-Key header is missing or longer than
	// MaxKeyLength. Mapped to 400 MISSING_IDEMPOTENCY_KEY.
	ErrMissingKey = errors.New("missing or invalid Idempotency-Key header")
	// ErrKeyReused: the same key was sent with a DIFFERENT request (another
	// hold_id). That is a client bug; replaying the old answer would be
	// wrong. Mapped to 422 IDEMPOTENCY_KEY_REUSED.
	ErrKeyReused = errors.New("idempotency key reused with a different request")
	// ErrInProgress: a request with this key is running right now (its
	// lease is still valid). Mapped to 409 REQUEST_IN_PROGRESS: retry later.
	ErrInProgress = errors.New("a request with this idempotency key is in progress")
)

// Kind says what Begin decided.
type Kind int

const (
	// Proceed: this request now owns the key (a new key, or a takeover of
	// an expired lease). Run the operation, then Complete or Release.
	Proceed Kind = iota + 1
	// Replay: the key is COMPLETED; send back Stored exactly.
	Replay
)

// Response is a stored HTTP response.
type Response struct {
	Status int
	Body   []byte // JSON
}

// Claim is the result of Begin.
type Claim struct {
	Kind   Kind
	Token  string   // the lease token, when Kind == Proceed
	Stored Response // the stored response, when Kind == Replay
	// TookOver is true when Proceed came from taking over an expired lease
	// (the earlier request crashed or gave the key back), false for a new key.
	TookOver bool
}

// Store keeps idempotency keys in the idempotency_keys table.
type Store struct {
	pool         *pgxpool.Pool
	lease        time.Duration
	queryTimeout time.Duration
}

// NewStore returns a store. lease (IDEMPOTENCY_LEASE, default 30s) is how
// long a request owns a key before another request may take it over. It
// must be longer than one whole confirm (two transactions plus the payment
// timeout), or a slow-but-alive request would lose its key while working.
func NewStore(pool *pgxpool.Pool, lease, queryTimeout time.Duration) *Store {
	return &Store{pool: pool, lease: lease, queryTimeout: queryTimeout}
}

// RequestHash fingerprints a request: method, path and the CANONICAL body
// (the decoded fields, not the raw bytes, so whitespace or key order do
// not matter). Same key + different hash = ErrKeyReused.
func RequestHash(method, path, canonicalBody string) string {
	sum := sha256.Sum256([]byte(method + " " + path + "\n" + canonicalBody))
	return hex.EncodeToString(sum[:])
}

// Begin claims the key for this request, or tells it to replay / back off.
//
// THE LEASE: a request that takes a key sets locked_until = NOW() + lease
// and a random lease_token. If the server crashes mid-request, nobody ever
// calls Complete, but the lease runs out, and the client's retry takes the
// key over and runs the operation again. That re-run is safe because the
// operation itself is idempotent underneath (one PENDING payment per hold,
// one charge_key per payment, a gateway that never charges a key twice).
func (s *Store) Begin(ctx context.Context, userID, key, requestHash string) (Claim, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	// A key can change state between our statements (another request may
	// insert, complete or take it over), so we retry the decision a few times.
	for attempt := 0; attempt < 3; attempt++ {
		token, err := newToken()
		if err != nil {
			return Claim{}, err
		}

		// 1. A brand-new key: insert it as ours. ON CONFLICT DO NOTHING makes
		//    concurrent first requests safe: exactly one insert wins.
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO idempotency_keys (user_id, key, request_hash, status, lease_token, locked_until)
			VALUES ($1, $2, $3, 'IN_PROGRESS', $4, NOW() + make_interval(secs => $5))
			ON CONFLICT (user_id, key) DO NOTHING`,
			userID, key, requestHash, token, s.lease.Seconds())
		if err != nil {
			return Claim{}, storeError(err, "insert idempotency key")
		}
		if tag.RowsAffected() == 1 {
			return Claim{Kind: Proceed, Token: token}, nil
		}

		// 2. The key exists. Look at it.
		var storedHash, status string
		var leaseValid bool
		var respStatus *int
		var respBody []byte
		err = s.pool.QueryRow(ctx, `
			SELECT request_hash, status, COALESCE(locked_until > NOW(), false), response_status, response_body
			FROM idempotency_keys WHERE user_id = $1 AND key = $2`, userID, key).
			Scan(&storedHash, &status, &leaseValid, &respStatus, &respBody)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // vanished between our statements; decide again
		}
		if err != nil {
			return Claim{}, storeError(err, "read idempotency key")
		}

		switch {
		case storedHash != requestHash:
			return Claim{}, ErrKeyReused
		case status == "COMPLETED" && respStatus != nil:
			return Claim{Kind: Replay, Stored: Response{Status: *respStatus, Body: respBody}}, nil
		case leaseValid:
			return Claim{}, ErrInProgress
		}

		// 3. IN_PROGRESS with an EXPIRED lease: the owner crashed or gave the
		//    key back (unknown outcome). Take it over, but only if it is
		//    still expired: two retries racing here cannot both win.
		tag, err = s.pool.Exec(ctx, `
			UPDATE idempotency_keys
			SET lease_token = $3, locked_until = NOW() + make_interval(secs => $4), updated_at = NOW()
			WHERE user_id = $1 AND key = $2
			  AND status = 'IN_PROGRESS' AND (locked_until IS NULL OR locked_until <= NOW())`,
			userID, key, token, s.lease.Seconds())
		if err != nil {
			return Claim{}, storeError(err, "take over idempotency key")
		}
		if tag.RowsAffected() == 1 {
			return Claim{Kind: Proceed, Token: token, TookOver: true}, nil
		}
		// Someone else completed or took it over in between: decide again.
	}
	return Claim{}, ErrInProgress
}

// Complete stores the FINAL response for the key, but ONLY if this request
// still holds the lease (lease_token = token). It reports whether it stored.
//
// Why the token check: after a takeover, TWO requests can be running for
// one key: the slow original (whose lease expired) and the retry that took
// it over. Without the check, the slow one could finish last and overwrite
// the stored answer. With it, only the current lease holder writes; the
// other just returns its own result to its own client. Running twice is
// safe only because of the layers below: one PENDING payment per hold (both
// runs share it), one charge_key per payment (the gateway charges once), and
// Tx B never refunding a payment that already backs a booking.
func (s *Store) Complete(ctx context.Context, userID, key, token string, resp Response) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.queryTimeout)
	defer cancel()

	tag, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'COMPLETED', response_status = $4, response_body = $5,
		    lease_token = NULL, locked_until = NULL, updated_at = NOW()
		WHERE user_id = $1 AND key = $2 AND lease_token = $3 AND status = 'IN_PROGRESS'`,
		userID, key, token, resp.Status, resp.Body)
	if err != nil {
		return false, storeError(err, "complete idempotency key")
	}
	return tag.RowsAffected() == 1, nil
}

// Release gives the key back WITHOUT a stored response, so the next request
// with this key runs the operation again. Used when the outcome is unknown
// (504 PAYMENT_PENDING, 503, 500): storing it would replay "unknown" forever,
// while a retry can drive the payment to a final answer. The row keeps its
// request_hash, so a retry with a different body still gets 422.
func (s *Store) Release(ctx context.Context, userID, key, token string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.queryTimeout)
	defer cancel()

	_, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET locked_until = NOW(), lease_token = NULL, updated_at = NOW()
		WHERE user_id = $1 AND key = $2 AND lease_token = $3 AND status = 'IN_PROGRESS'`,
		userID, key, token)
	if err != nil {
		return storeError(err, "release idempotency key")
	}
	return nil
}

// DeleteCompleted deletes up to limit COMPLETED keys whose response was
// stored more than retention ago, and returns how many it deleted. Used by
// the worker's cleanup job so the table does not grow forever.
//
// The trade-off: once a key is deleted, a request with it is treated as NEW
// (it would run again). So retention (IDEMPOTENCY_RETENTION, default 24h)
// must be longer than any realistic client retry window. IN_PROGRESS keys
// are never deleted: they belong to a running request, or carry a request
// hash a retry still needs (to answer 422 for a different body).
func (s *Store) DeleteCompleted(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	tag, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE (user_id, key) IN (
			SELECT user_id, key FROM idempotency_keys
			WHERE status = 'COMPLETED' AND updated_at <= NOW() - make_interval(secs => $1)
			LIMIT $2
			FOR UPDATE SKIP LOCKED)`, retention.Seconds(), limit)
	if err != nil {
		return 0, storeError(err, "delete old idempotency keys")
	}
	return tag.RowsAffected(), nil
}

// newToken returns a random 128-bit lease token.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("lease token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// storeError wraps database errors; timeouts become booking.ErrBusy (503),
// exactly like in the booking repository.
func storeError(err error, op string) error {
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return fmt.Errorf("%s: %w: %w", op, booking.ErrBusy, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
