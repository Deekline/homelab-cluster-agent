package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token"

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func newTestMux(startupScript, shutdownScript string) (http.Handler, *jobState) {
	state := &jobState{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, testToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, state.snapshot())
	})
	mux.HandleFunc("POST /hooks/startup", jobHandler(state, "startup", startupScript, testToken))
	mux.HandleFunc("POST /hooks/shutdown", jobHandler(state, "shutdown", shutdownScript, testToken))
	return mux, state
}

func doRequest(t *testing.T, mux http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("X-Agent-Token", token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// waitUntilIdle polls /status until the job is no longer running or the
// timeout elapses, returning the final decoded status.
func waitUntilIdle(t *testing.T, mux http.Handler) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec := doRequest(t, mux, http.MethodGet, "/status", testToken)
		var status map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		if running, _ := status["running"].(bool); !running && status["finished_at"] != "" {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for job to finish")
	return nil
}

func TestAuthorized(t *testing.T) {
	cases := []struct {
		name  string
		got   string
		want  string
		valid bool
	}{
		{"matching token", "secret", "secret", true},
		{"wrong token", "wrong", "secret", false},
		{"empty header", "", "secret", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			if c.got != "" {
				req.Header.Set("X-Agent-Token", c.got)
			}
			if authorized(req, c.want) != c.valid {
				t.Errorf("authorized() = %v, want %v", !c.valid, c.valid)
			}
		})
	}
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	mux, _ := newTestMux("/nonexistent-startup.sh", "/nonexistent-shutdown.sh")
	rec := doRequest(t, mux, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
}

func TestJobHandlerRequiresToken(t *testing.T) {
	mux, _ := newTestMux("/nonexistent-startup.sh", "/nonexistent-shutdown.sh")
	rec := doRequest(t, mux, http.MethodPost, "/hooks/startup", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	rec = doRequest(t, mux, http.MethodPost, "/hooks/startup", "wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestJobHandlerMissingScript(t *testing.T) {
	mux, _ := newTestMux("/nonexistent-startup.sh", "/nonexistent-shutdown.sh")
	rec := doRequest(t, mux, http.MethodPost, "/hooks/startup", testToken)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestJobHandlerRunsScriptAndRecordsSuccess(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "startup.sh", "#!/bin/sh\necho hello\nexit 0\n")
	mux, _ := newTestMux(script, "/nonexistent-shutdown.sh")

	rec := doRequest(t, mux, http.MethodPost, "/hooks/startup", testToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}

	status := waitUntilIdle(t, mux)
	if got := status["exit_code"]; got != float64(0) {
		t.Errorf("exit_code = %v, want 0", got)
	}
	if got := status["output_tail"]; got != "hello\n" {
		t.Errorf("output_tail = %q, want %q", got, "hello\n")
	}
}

func TestJobHandlerRecordsNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "shutdown.sh", "#!/bin/sh\necho oops\nexit 7\n")
	mux, _ := newTestMux("/nonexistent-startup.sh", script)

	rec := doRequest(t, mux, http.MethodPost, "/hooks/shutdown", testToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	status := waitUntilIdle(t, mux)
	if got := status["exit_code"]; got != float64(7) {
		t.Errorf("exit_code = %v, want 7", got)
	}
	if status["error"] == "" {
		t.Errorf("expected error field to be set for non-zero exit")
	}
}

func TestJobHandlerRejectsConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "slow.sh", "#!/bin/sh\nsleep 0.3\necho done\n")
	mux, _ := newTestMux(script, script)

	first := doRequest(t, mux, http.MethodPost, "/hooks/startup", testToken)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first request status = %d, want 202", first.Code)
	}

	second := doRequest(t, mux, http.MethodPost, "/hooks/shutdown", testToken)
	if second.Code != http.StatusConflict {
		t.Fatalf("second request status = %d, want 409", second.Code)
	}

	waitUntilIdle(t, mux)
}

func TestJobHandlerStreamsOutputWhileRunning(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "slow.sh", "#!/bin/sh\necho first\nsleep 0.5\necho second\n")
	mux, _ := newTestMux(script, "/nonexistent-shutdown.sh")

	rec := doRequest(t, mux, http.MethodPost, "/hooks/startup", testToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	// Poll until we observe "first" while the job is still running, proving
	// output_tail updates live rather than only after the process exits.
	deadline := time.Now().Add(2 * time.Second)
	sawFirstWhileRunning := false
	for time.Now().Before(deadline) {
		statusRec := doRequest(t, mux, http.MethodGet, "/status", testToken)
		var status map[string]any
		if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		running, _ := status["running"].(bool)
		tail, _ := status["output_tail"].(string)
		if running && strings.Contains(tail, "first") {
			sawFirstWhileRunning = true
			break
		}
		if !running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !sawFirstWhileRunning {
		t.Fatal("expected /status to show partial output while the job was still running")
	}

	status := waitUntilIdle(t, mux)
	if got := status["output_tail"]; got != "first\nsecond\n" {
		t.Errorf("output_tail = %q, want %q", got, "first\nsecond\n")
	}
}

func TestJobStateWriteTruncatesToRollingTail(t *testing.T) {
	state := &jobState{}
	state.start("test")

	if _, err := state.Write([]byte(strings.Repeat("a", maxOutputBytes-5))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := state.Write([]byte(strings.Repeat("b", 10))); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := state.snapshot()["output_tail"].(string)
	if len(got) != maxOutputBytes {
		t.Fatalf("len(output_tail) = %d, want %d", len(got), maxOutputBytes)
	}
	if !strings.HasSuffix(got, strings.Repeat("b", 10)) {
		t.Fatalf("expected output_tail to end with the most recent writes")
	}
}

func TestStatusRequiresToken(t *testing.T) {
	mux, _ := newTestMux("/nonexistent-startup.sh", "/nonexistent-shutdown.sh")
	rec := doRequest(t, mux, http.MethodGet, "/status", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
