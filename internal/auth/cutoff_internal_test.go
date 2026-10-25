package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestACutOffEndsWithItsRequest: writes cut off during a request are let
// go again once its handler returns, so the response is written and the
// connection serves the next request.
func TestACutOffEndsWithItsRequest(t *testing.T) {
	srv := httptest.NewServer(CutOff(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A deadline already past: anything written before the handler
		// returns fails. Written after, the response stands only if the
		// deadline went with the request.
		cutOffWrites(r.Context(), 0)
	})))
	defer srv.Close()
	for i := range 2 {
		resp, err := srv.Client().Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request %d: %d", i, resp.StatusCode)
		}
	}
}
