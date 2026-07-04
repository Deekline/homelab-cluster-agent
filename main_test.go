package main

import (
	"encoding/json"
	"net"
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
	mux.HandleFunc("POST /hooks/startup", jobHandler(state, "startup", startupScript, testToken, nil))
	mux.HandleFunc("POST /hooks/shutdown", jobHandler(state, "shutdown", shutdownScript, testToken, nil))
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

func TestStatusRequiresToken(t *testing.T) {
	mux, _ := newTestMux("/nonexistent-startup.sh", "/nonexistent-shutdown.sh")
	rec := doRequest(t, mux, http.MethodGet, "/status", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMagicPacketFormat(t *testing.T) {
	packet, err := magicPacket("AA:BB:CC:DD:EE:FF")
	if err != nil {
		t.Fatalf("magicPacket: %v", err)
	}
	if len(packet) != 102 {
		t.Fatalf("len(packet) = %d, want 102", len(packet))
	}
	for i := 0; i < 6; i++ {
		if packet[i] != 0xFF {
			t.Fatalf("packet[%d] = %#x, want 0xFF", i, packet[i])
		}
	}
	want := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	for rep := 0; rep < 16; rep++ {
		got := packet[6+rep*6 : 6+rep*6+6]
		for i, b := range got {
			if b != want[i] {
				t.Fatalf("mac repetition %d = % x, want % x", rep, got, want)
			}
		}
	}
}

func TestMagicPacketInvalidMAC(t *testing.T) {
	if _, err := magicPacket("not-a-mac"); err == nil {
		t.Fatal("expected error for invalid MAC, got nil")
	}
}

func TestWolPreRunNilWhenUnconfigured(t *testing.T) {
	cfg := config{wolMAC: ""}
	if preRun := wolPreRun(cfg); preRun != nil {
		t.Fatal("expected wolPreRun to be nil when WOL_MAC is unset")
	}
}

func TestWolPreRunSendsPacket(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer pc.Close()

	cfg := config{wolMAC: "AA:BB:CC:DD:EE:FF", wolBroadcast: pc.LocalAddr().String()}
	preRun := wolPreRun(cfg)
	if preRun == nil {
		t.Fatal("expected non-nil preRun when WOL_MAC is set")
	}

	msg, err := preRun()
	if err != nil {
		t.Fatalf("preRun: %v", err)
	}
	if msg == "" {
		t.Fatal("expected non-empty confirmation message")
	}

	buf := make([]byte, 200)
	pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 102 {
		t.Fatalf("received %d bytes, want 102", n)
	}
}

func TestJobHandlerRunsWolPreRunBeforeScript(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "startup.sh", "#!/bin/sh\necho hello\nexit 0\n")

	state := &jobState{}
	mux := http.NewServeMux()
	preRun := func() (string, error) { return "WOL: sent magic packet to test", nil }
	mux.HandleFunc("POST /hooks/startup", jobHandler(state, "startup", script, testToken, preRun))
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, testToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, state.snapshot())
	})

	rec := doRequest(t, mux, http.MethodPost, "/hooks/startup", testToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	status := waitUntilIdle(t, mux)
	output, _ := status["output_tail"].(string)
	if !strings.Contains(output, "WOL: sent magic packet to test") {
		t.Errorf("output_tail = %q, want it to contain the WOL pre-run message", output)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("output_tail = %q, want it to also contain script output", output)
	}
}
