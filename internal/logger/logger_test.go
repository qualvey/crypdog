package logger

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"trace":   LevelTrace,
		"TRACE":   LevelTrace,
		"debug":   LevelDebug,
		"DEBUG":   LevelDebug,
		"info":    LevelInfo,
		"INFO":    LevelInfo,
		"":        LevelInfo,
		"warn":    LevelWarn,
		"warning": LevelWarn,
		"WARN":    LevelWarn,
		"error":   LevelError,
		"fatal":   LevelError,
		"panic":   LevelError,
		"unknown": LevelInfo,
	}

	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestInitHonorsLevel(t *testing.T) {
	oldLevel := currentLevel
	buf := &bytes.Buffer{}
	SetOutput(buf)
	defer ResetOutput()
	defer func() { currentLevel = oldLevel }()

	SetLevel("warn")
	Debug("debug message")
	Info("info message")
	Warn("warn message")
	Error("error message")

	out := buf.String()
	if strings.Contains(out, "debug message") {
		t.Fatalf("debug message should be filtered out at warn level: %s", out)
	}
	if strings.Contains(out, "info message") {
		t.Fatalf("info message should be filtered out at warn level: %s", out)
	}
	if !strings.Contains(out, "warn message") || !strings.Contains(out, "error message") {
		t.Fatalf("warn and error messages should remain: %s", out)
	}
}

func TestDynamicLevelAndSlog(t *testing.T) {
	buf := &bytes.Buffer{}
	SetOutput(buf)
	defer ResetOutput()

	SetLevel("debug")
	if !IsDebug() {
		t.Fatalf("expected IsDebug to be true")
	}
	slog.Debug("slog debug message")
	Debug("logger debug message")

	out := buf.String()
	if !strings.Contains(out, "slog debug message") {
		t.Fatalf("slog debug should be output when level is debug: %s", out)
	}
	if !strings.Contains(out, "logger debug message") {
		t.Fatalf("logger debug should be output when level is debug: %s", out)
	}

	buf.Reset()
	SetLevel("info")
	if IsDebug() {
		t.Fatalf("expected IsDebug to be false")
	}
	slog.Debug("slog debug message 2")
	Debug("logger debug message 2")
	Info("logger info message 2")

	out = buf.String()
	if strings.Contains(out, "slog debug message 2") || strings.Contains(out, "logger debug message 2") {
		t.Fatalf("debug messages should be filtered at info level: %s", out)
	}
	if !strings.Contains(out, "logger info message 2") {
		t.Fatalf("info message should be present: %s", out)
	}
}

func TestConcurrentSetLevel(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				SetLevel("debug")
				_ = IsDebug()
			} else {
				SetLevel("info")
				_ = GetLevel()
			}
		}(i)
	}
	wg.Wait()
}
