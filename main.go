package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
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

func (s *jobState) finish(exitCode int, output []byte, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.finishedAt = time.Now()
	s.exitCode = exitCode
	if len(output) > maxOutputBytes {
		output = output[len(output)-maxOutputBytes:]
	}
	s.output.Reset()
	s.output.Write(output)
	if runErr != nil {
		s.lastErr = runErr.Error()
	}
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
	wolMAC         string
	wolBroadcast   string
}

func loadConfig() config {
	cfg := config{
		addr:           getenv("LISTEN_ADDR", ":9090"),
		token:          os.Getenv("AGENT_TOKEN"),
		startupScript:  getenv("STARTUP_SCRIPT", "/opt/homelab/scripts/startup.sh"),
		shutdownScript: getenv("SHUTDOWN_SCRIPT", "/opt/homelab/scripts/shutdown.sh"),
		wolMAC:         getenv("WOL_MAC", ""),
		wolBroadcast:   getenv("WOL_BROADCAST_ADDR", "255.255.255.255:9"),
	}
	if cfg.token == "" {
		log.Fatal("AGENT_TOKEN must be set")
	}
	if cfg.wolMAC != "" {
		if _, err := net.ParseMAC(cfg.wolMAC); err != nil {
			log.Fatalf("invalid WOL_MAC %q: %v", cfg.wolMAC, err)
		}
	}
	return cfg
}

// magicPacket builds the standard 102-byte Wake-on-LAN payload for mac:
// six 0xFF bytes followed by the target MAC address repeated 16 times.
func magicPacket(mac string) ([]byte, error) {
	hwAddr, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("parse mac: %w", err)
	}
	if len(hwAddr) != 6 {
		return nil, fmt.Errorf("mac %q must be 6 bytes, got %d", mac, len(hwAddr))
	}
	packet := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		packet = append(packet, 0xFF)
	}
	for i := 0; i < 16; i++ {
		packet = append(packet, hwAddr...)
	}
	return packet, nil
}

// sendMagicPacket wakes a host by UDP-broadcasting a magic packet for mac to
// broadcastAddr (host:port, typically the subnet broadcast address on port 9).
func sendMagicPacket(mac, broadcastAddr string) error {
	packet, err := magicPacket(mac)
	if err != nil {
		return err
	}
	conn, err := net.Dial("udp", broadcastAddr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", broadcastAddr, err)
	}
	defer conn.Close()
	if _, err := conn.Write(packet); err != nil {
		return fmt.Errorf("write to %s: %w", broadcastAddr, err)
	}
	return nil
}

// wolPreRun returns a runJob pre-step that sends a WOL magic packet, or nil
// if no target MAC is configured (WOL stays a no-op by default).
func wolPreRun(cfg config) func() (string, error) {
	if cfg.wolMAC == "" {
		return nil
	}
	return func() (string, error) {
		if err := sendMagicPacket(cfg.wolMAC, cfg.wolBroadcast); err != nil {
			return "", fmt.Errorf("WOL: failed to send magic packet to %s: %w", cfg.wolMAC, err)
		}
		return fmt.Sprintf("WOL: sent magic packet to %s via %s", cfg.wolMAC, cfg.wolBroadcast), nil
	}
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
	mux.HandleFunc("POST /hooks/startup", jobHandler(state, "startup", cfg.startupScript, cfg.token, wolPreRun(cfg)))
	mux.HandleFunc("POST /hooks/shutdown", jobHandler(state, "shutdown", cfg.shutdownScript, cfg.token, nil))

	log.Printf("homelab-cluster-agent listening on %s", cfg.addr)
	if err := http.ListenAndServe(cfg.addr, mux); err != nil {
		log.Fatal(err)
	}
}

func authorized(r *http.Request, token string) bool {
	got := r.Header.Get("X-Agent-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func jobHandler(state *jobState, name, scriptPath, token string, preRun func() (string, error)) http.HandlerFunc {
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
		go runJob(state, name, scriptPath, preRun)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"status": "started", "job": name})
	}
}

func runJob(state *jobState, name, scriptPath string, preRun func() (string, error)) {
	var out bytes.Buffer
	if preRun != nil {
		msg, err := preRun()
		if msg != "" {
			log.Print(msg)
			out.WriteString(msg + "\n")
		}
		if err != nil {
			log.Printf("job %q pre-run step failed: %v", name, err)
			out.WriteString(err.Error() + "\n")
		}
	}

	cmd := exec.Command(scriptPath)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	state.finish(exitCode, out.Bytes(), err)
	log.Printf("job %q finished with exit code %d", name, exitCode)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
