// server_test.go tests the mock gateway itself through httptest: idempotent
// charges, the 422 on a reused key, forced behaviours, the "money taken,
// answer lost" timeout, idempotent refunds, stats and reset.
package paymentmock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	mock := New(Options{SlowDelay: 300 * time.Millisecond, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	return mock, srv
}

// post sends a JSON body and returns the status and decoded body.
func post(t *testing.T, client *http.Client, url, body, behavior string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	require.NoError(t, err)
	if behavior != "" {
		req.Header.Set("X-Mock-Behavior", behavior)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestCharge_SameKeyIsChargedOnce(t *testing.T) {
	mock, srv := newTestServer(t)
	body := `{"idempotency_key":"pay-1","amount_paise":25000,"reference":"hold-1"}`

	status1, first := post(t, srv.Client(), srv.URL+"/charges", body, "")
	status2, second := post(t, srv.Client(), srv.URL+"/charges", body, "decline")

	assert.Equal(t, http.StatusOK, status1)
	assert.Equal(t, http.StatusOK, status2)
	assert.Equal(t, first, second, "a repeated key returns the stored result, whatever behaviour is asked")
	st := mock.Stats()
	assert.Equal(t, 1, st.TotalCharges)
	assert.Equal(t, 2, st.ChargeRequests["pay-1"])
}

func TestCharge_ConcurrentSameKey_OneCharge(t *testing.T) {
	mock, srv := newTestServer(t)
	body := `{"idempotency_key":"pay-9","amount_paise":25000}`
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, srv.Client(), srv.URL+"/charges", body, "")
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, mock.Stats().TotalCharges)
}

func TestCharge_SameKeyDifferentAmount_422(t *testing.T) {
	_, srv := newTestServer(t)
	post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-1","amount_paise":25000}`, "")

	status, _ := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-1","amount_paise":99}`, "")

	assert.Equal(t, http.StatusUnprocessableEntity, status)
}

func TestCharge_Declined(t *testing.T) {
	mock, srv := newTestServer(t)

	_, out := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-1","amount_paise":25000}`, BehaviorDecline)

	assert.Equal(t, StatusDeclined, out["status"])
	assert.Equal(t, 0, mock.Stats().MoneyTaken)
}

func TestCharge_QueuedBehaviorIsUsedOnce(t *testing.T) {
	_, srv := newTestServer(t)
	status, _ := post(t, srv.Client(), srv.URL+"/debug/next-behavior", `{"behavior":"decline","count":1}`, "")
	require.Equal(t, http.StatusOK, status)

	_, first := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"a","amount_paise":1}`, "")
	_, second := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"b","amount_paise":1}`, "")

	assert.Equal(t, StatusDeclined, first["status"])
	assert.Equal(t, StatusSucceeded, second["status"])
}

// TestCharge_Timeout_MoneyTakenAnswerLost: the client gives up, but the charge
// exists. Retrying with the same key returns it, without a second charge.
func TestCharge_Timeout_MoneyTakenAnswerLost(t *testing.T) {
	mock, srv := newTestServer(t)
	impatient := &http.Client{Timeout: 200 * time.Millisecond}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/charges",
		bytes.NewBufferString(`{"idempotency_key":"pay-7","amount_paise":25000}`))
	require.NoError(t, err)
	req.Header.Set("X-Mock-Behavior", BehaviorTimeout)

	_, err = impatient.Do(req)
	require.Error(t, err, "the client must time out")

	_, out := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-7","amount_paise":25000}`, "")
	assert.Equal(t, StatusSucceeded, out["status"], "the money was taken")
	assert.Equal(t, 1, mock.Stats().TotalCharges)
}

func TestCharge_Slow(t *testing.T) {
	_, srv := newTestServer(t)
	start := time.Now()

	_, out := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"s","amount_paise":1}`, BehaviorSlow)

	assert.Equal(t, StatusSucceeded, out["status"])
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
}

func TestRefund_IsIdempotent(t *testing.T) {
	mock, srv := newTestServer(t)
	_, charge := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-1","amount_paise":25000}`, "")
	chargeID := charge["charge_id"].(string)

	_, r1 := post(t, srv.Client(), srv.URL+"/refunds", `{"idempotency_key":"refund-1","charge_id":"`+chargeID+`"}`, "")
	_, r2 := post(t, srv.Client(), srv.URL+"/refunds", `{"idempotency_key":"refund-1","charge_id":"`+chargeID+`"}`, "")
	_, r3 := post(t, srv.Client(), srv.URL+"/refunds", `{"idempotency_key":"other","charge_id":"`+chargeID+`"}`, "")

	assert.Equal(t, StatusRefunded, r1["status"])
	assert.Equal(t, r1, r2)
	assert.Equal(t, r1["refund_id"], r3["refund_id"], "a charge is refunded at most once")
	assert.Equal(t, 1, mock.Stats().TotalRefunds)
}

func TestGetChargeAndReset(t *testing.T) {
	mock, srv := newTestServer(t)
	post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-1","amount_paise":25000}`, "")
	post(t, srv.Client(), srv.URL+"/debug/next-behavior", `{"behavior":"decline","count":3}`, "")

	resp, err := srv.Client().Get(srv.URL + "/charges/pay-1")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	status, _ := post(t, srv.Client(), srv.URL+"/debug/reset", `{}`, "")
	require.Equal(t, http.StatusOK, status)

	resp, err = srv.Client().Get(srv.URL + "/charges/pay-1")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, 0, mock.Stats().TotalCharges)
	_, out := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"x","amount_paise":1}`, "")
	assert.Equal(t, StatusSucceeded, out["status"], "reset must also clear queued behaviours")
}

func TestVoid_UnknownKey_BlocksLaterCharge(t *testing.T) {
	mock, srv := newTestServer(t)

	status, voided := post(t, srv.Client(), srv.URL+"/charges/pay-9/void", `{}`, "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, StatusVoided, voided["status"])

	// A late request with the voided key must not take money.
	_, late := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-9","amount_paise":25000}`, "")
	assert.Equal(t, StatusVoided, late["status"])
	st := mock.Stats()
	assert.Equal(t, 0, st.MoneyTaken)
	assert.Equal(t, 0, st.TotalCharges)
	assert.Equal(t, 1, st.TotalVoids)
}

// TestVoid_AfterCharge_ReturnsRealResult is the void race from the other side:
// the slow charge got there first, so the void reports "succeeded".
func TestVoid_AfterCharge_ReturnsRealResult(t *testing.T) {
	mock, srv := newTestServer(t)
	post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-9","amount_paise":25000}`, "")

	_, out := post(t, srv.Client(), srv.URL+"/charges/pay-9/void", `{}`, "")

	assert.Equal(t, StatusSucceeded, out["status"])
	assert.NotEmpty(t, out["charge_id"])
	assert.Equal(t, 0, mock.Stats().TotalVoids)
	assert.Equal(t, 1, mock.Stats().MoneyTaken)
}

func TestRefundFail_FailsTheNextNRefunds(t *testing.T) {
	mock, srv := newTestServer(t)
	_, charge := post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-1","amount_paise":25000}`, "")
	post(t, srv.Client(), srv.URL+"/debug/next-behavior", `{"behavior":"refund_fail","count":2}`, "")
	body := `{"idempotency_key":"refund-1","charge_id":"` + charge["charge_id"].(string) + `"}`

	s1, _ := post(t, srv.Client(), srv.URL+"/refunds", body, "")
	s2, _ := post(t, srv.Client(), srv.URL+"/refunds", body, "")
	s3, out := post(t, srv.Client(), srv.URL+"/refunds", body, "")

	assert.Equal(t, http.StatusInternalServerError, s1)
	assert.Equal(t, http.StatusInternalServerError, s2)
	assert.Equal(t, http.StatusOK, s3)
	assert.Equal(t, StatusRefunded, out["status"])
	assert.Equal(t, 1, mock.Stats().TotalRefunds)
}

func TestMetricsEndpoint(t *testing.T) {
	_, srv := newTestServer(t)
	post(t, srv.Client(), srv.URL+"/charges", `{"idempotency_key":"pay-1","amount_paise":25000}`, "")

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Contains(t, string(body), "seatlock_mock_gateway_money_taken 1")
	assert.Contains(t, string(body), "seatlock_mock_gateway_refunds 0")
}
