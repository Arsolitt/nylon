package logging

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	t.Setenv(EnvLogLevel, "warn")

	lvl, err := Resolve("debug", false)
	if err != nil || lvl != slog.LevelDebug {
		t.Fatalf("flag should win: %v, %v", lvl, err)
	}

	lvl, err = Resolve("", false)
	if err != nil || lvl != slog.LevelWarn {
		t.Fatalf("env should apply when flag empty: %v, %v", lvl, err)
	}

	t.Setenv(EnvLogLevel, "")
	lvl, err = Resolve("", true)
	if err != nil || lvl != slog.LevelDebug {
		t.Fatalf("verbose should imply debug: %v, %v", lvl, err)
	}

	lvl, err = Resolve("", false)
	if err != nil || lvl != slog.LevelInfo {
		t.Fatalf("default should be info: %v, %v", lvl, err)
	}

	if _, err := Resolve("bogus", false); err == nil || !strings.Contains(err.Error(), "debug, info, warn, error") {
		t.Fatalf("invalid flag level should error naming valid values, got %v", err)
	}

	t.Setenv(EnvLogLevel, "nope")
	if _, err := Resolve("", false); err == nil || !strings.Contains(err.Error(), "debug, info, warn, error") {
		t.Fatalf("invalid env level should error naming valid values, got %v", err)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"Warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Fatalf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Fatal("ParseLevel should reject unknown values")
	}
}

// captureStderr swaps os.Stderr for the duration of fn and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()

	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNewJSONStderr(t *testing.T) {
	out := captureStderr(t, func() {
		log, closer, err := New(Config{Component: "nylon", Node: "n1", Level: slog.LevelInfo, JSON: true})
		if err != nil {
			t.Fatal(err)
		}
		defer closer()
		log.Info("hello", "key", "value")
	})
	dec := json.NewDecoder(strings.NewReader(out))
	rec := map[string]any{}
	if err := dec.Decode(&rec); err != nil {
		t.Fatalf("stderr output is not parseable JSON: %v\n%s", err, out)
	}
	if rec["msg"] != "hello" || rec["component"] != "nylon" || rec["node"] != "n1" || rec["key"] != "value" {
		t.Fatalf("unexpected record: %v", rec)
	}
}

func TestNewTintStderr(t *testing.T) {
	out := captureStderr(t, func() {
		log, closer, err := New(Config{Component: "nylon-lb", Node: "lb-1", Level: slog.LevelInfo})
		if err != nil {
			t.Fatal(err)
		}
		defer closer()
		log.Warn("careful", "error", "boom")
	})
	plain := ansiRe.ReplaceAllString(out, "")
	if !strings.HasPrefix(plain, "WRN nylon-lb careful") {
		t.Fatalf("tint output should start with level + component prefix: %q", plain)
	}
	if !strings.Contains(plain, "component=nylon-lb") || !strings.Contains(plain, "node=lb-1") || !strings.Contains(plain, "error=boom") {
		t.Fatalf("tint output missing attrs: %q", plain)
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestNewFileSinkJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "nylon.log")
	log, closer, err := New(Config{Component: "nylon", Node: "n9", Level: slog.LevelInfo, FilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("to-file")
	closer()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("file sink is not JSON: %v\n%s", err, data)
	}
	if rec["component"] != "nylon" || rec["node"] != "n9" || rec["msg"] != "to-file" {
		t.Fatalf("unexpected file record: %v", rec)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("log file perm = %v, want 0600", st.Mode().Perm())
	}
}

func TestNewComponentRequired(t *testing.T) {
	if _, _, err := New(Config{}); err == nil {
		t.Fatal("empty component should error")
	}
}
