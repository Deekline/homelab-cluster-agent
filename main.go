package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

const maxOutputBytes = 64 * 1024

type jobState struct {
	mu         sync.Mutex
	running    bool
	name       string
	startedAt  time.Time
	finishedAt time.Time
	exitCode   int
	output     bytes.Buffer
	lastErr    string
}

func (s *jobState) start(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	s.name = name
	s.startedAt = time.Now()
	s.finishedAt = time.Time{}
	s.exitCode = 0
	s.output.Reset()
	s.lastErr = ""
	return true
}

func (s *jobState) finish(exitCode int, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.finishedAt = time.Now()
	s.exitCode = exitCode
	if runErr != nil {
		s.lastErr = runErr.Error()
	}
}

// Write implements io.Writer so a running job's combined stdout/stderr can
// be wired directly into cmd.Stdout/cmd.Stderr and show up in /status
// (output_tail) while the job is still in flight, not just after it exits.
// Enforces maxOutputBytes as a rolling tail, same as before.
func (s *jobState) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output.Write(p)
	if s.output.Len() > maxOutputBytes {
		tail := append([]byte(nil), s.output.Bytes()[s.output.Len()-maxOutputBytes:]...)
		s.output.Reset()
		s.output.Write(tail)
	}
	return len(p), nil
}

func (s *jobState) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"running":     s.running,
		"job":         s.name,
		"started_at":  formatTime(s.startedAt),
		"finished_at": formatTime(s.finishedAt),
		"exit_code":   s.exitCode,
		"error":       s.lastErr,
		"output_tail": s.output.String(),
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

type config struct {
	addr           string
	token          string
	startupScript  string
	shutdownScript string
}

func loadConfig() config {
	cfg := config{
		addr:           getenv("LISTEN_ADDR", ":9090"),
		token:          os.Getenv("AGENT_TOKEN"),
		startupScript:  getenv("STARTUP_SCRIPT", "/opt/homelab/scripts/startup.sh"),
		shutdownScript: getenv("SHUTDOWN_SCRIPT", "/opt/homelab/scripts/shutdown.sh"),
	}
	if cfg.token == "" {
		log.Fatal("AGENT_TOKEN must be set")
	}
	return cfg
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := loadConfig()
	state := &jobState{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, cfg.token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, state.snapshot())
	})
	mux.HandleFunc("POST /hooks/startup", jobHandler(state, "startup", cfg.startupScript, cfg.token))
	mux.HandleFunc("POST /hooks/shutdown", jobHandler(state, "shutdown", cfg.shutdownScript, cfg.token))

	log.Printf("homelab-cluster-agent listening on %s", cfg.addr)
	if err := http.ListenAndServe(cfg.addr, mux); err != nil {
		log.Fatal(err)
	}
}

func authorized(r *http.Request, token string) bool {
	got := r.Header.Get("X-Agent-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func jobHandler(state *jobState, name, scriptPath, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if _, err := os.Stat(scriptPath); err != nil {
			http.Error(w, fmt.Sprintf("script not found: %s", scriptPath), http.StatusInternalServerError)
			return
		}
		if !state.start(name) {
			http.Error(w, "a job is already running", http.StatusConflict)
			return
		}
		log.Printf("starting job %q (%s)", name, scriptPath)
		go runJob(state, name, scriptPath)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"status": "started", "job": name})
	}
}

func runJob(state *jobState, name, scriptPath string) {
	cmd := exec.Command(scriptPath)
	cmd.Stdout = state
	cmd.Stderr = state
	err := cmd.Run()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	state.finish(exitCode, err)
	log.Printf("job %q finished with exit code %d", name, exitCode)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
