package execlog

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/deepak-science/kestrel/internal/profile"
)

// Entry represents a single command execution in the log.
type Entry struct {
	Timestamp  time.Time `json:"timestamp"`
	Command    string    `json:"command"`
	Args       []string  `json:"args"`
	Dir        string    `json:"dir,omitempty"`
	ExitCode   int       `json:"exit_code"`
	DurationMs int64     `json:"duration_ms"`
}

var (
	mu      sync.Mutex
	logFile *os.File
	encoder *json.Encoder
)

// Init opens the exec log file for appending.
// Returns a cleanup function to close the file.
func Init() (func(), error) {
	dir := profile.LogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}, err
	}

	f, err := os.OpenFile(dir+"/commands.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return func() {}, err
	}

	mu.Lock()
	logFile = f
	encoder = json.NewEncoder(f)
	mu.Unlock()

	return func() { _ = f.Close() }, nil
}

// Log writes a command execution entry to the log.
// No-op if Init has not been called.
func Log(entry Entry) {
	mu.Lock()
	defer mu.Unlock()
	if encoder == nil {
		return
	}
	entry.Args = Redact(entry.Args)
	_ = encoder.Encode(entry)
}
