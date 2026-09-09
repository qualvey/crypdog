// Package logger 提供统一的日志输出抽象，支持级别控制和文件落地。
package logger

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Level int

const (
	LevelTrace Level = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelTrace:
		return "TRACE"
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

func (l Level) SlogLevel() slog.Level {
	switch l {
	case LevelTrace, LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error", "fatal", "panic":
		return LevelError
	case "info", "":
		return LevelInfo
	default:
		return LevelInfo
	}
}

var (
	defaultWriter io.Writer      = os.Stderr
	std                          = log.New(defaultWriter, "", log.LstdFlags)
	mu            sync.RWMutex
	currentLevel  Level          = LevelInfo
	slogLevelVar  *slog.LevelVar = new(slog.LevelVar)
	logFileHandle *os.File
)

func init() {
	slogLevelVar.Set(slog.LevelInfo)
	handler := slog.NewTextHandler(defaultWriter, &slog.HandlerOptions{
		Level: slogLevelVar,
	})
	slog.SetDefault(slog.New(handler))
}

// Init 初始化日志系统（设置日志级别与可选的文件落地）
func Init(level string, output string, outfile string) {
	SetLevel(level)

	mu.Lock()
	defer mu.Unlock()

	// 清理旧的文件句柄
	if logFileHandle != nil {
		_ = logFileHandle.Close()
		logFileHandle = nil
	}

	var writers []io.Writer

	output = strings.ToLower(strings.TrimSpace(output))
	switch output {
	case "stdout":
		writers = append(writers, os.Stdout)
	case "file":
		// 仅写入文件
	default: // stderr 或未指定
		writers = append(writers, os.Stderr)
	}

	if outfile != "" {
		if err := os.MkdirAll(filepath.Dir(outfile), 0755); err == nil {
			if f, err := os.OpenFile(outfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
				logFileHandle = f
				writers = append(writers, f)
			}
		}
	}

	if len(writers) == 0 {
		writers = append(writers, os.Stderr)
	}

	var finalWriter io.Writer
	if len(writers) == 1 {
		finalWriter = writers[0]
	} else {
		finalWriter = io.MultiWriter(writers...)
	}

	std.SetOutput(finalWriter)
	handler := slog.NewTextHandler(finalWriter, &slog.HandlerOptions{
		Level: slogLevelVar,
	})
	slog.SetDefault(slog.New(handler))
}

// SetLevel 动态调整日志级别（线程安全，同时同步 slog）
func SetLevel(levelText string) {
	mu.Lock()
	defer mu.Unlock()

	parsed := ParseLevel(levelText)
	currentLevel = parsed
	slogLevelVar.Set(parsed.SlogLevel())
	slog.SetLogLoggerLevel(parsed.SlogLevel())
	std.SetPrefix("")
	std.SetFlags(log.LstdFlags)
}

// GetLevel 获取当前日志级别
func GetLevel() Level {
	mu.RLock()
	defer mu.RUnlock()
	return currentLevel
}

// IsDebug 检查当前是否开启了 Debug 或更低（Trace）日志级别
func IsDebug() bool {
	mu.RLock()
	defer mu.RUnlock()
	return currentLevel <= LevelDebug
}

func enabled(level Level) bool {
	mu.RLock()
	defer mu.RUnlock()
	return level >= currentLevel
}

func logf(level Level, format string, args ...interface{}) {
	if !enabled(level) {
		return
	}
	std.Printf("[%s] %s", level.String(), fmt.Sprintf(format, args...))
}

func Trace(format string, args ...interface{}) {
	logf(LevelTrace, format, args...)
}

func Debug(format string, args ...interface{}) {
	logf(LevelDebug, format, args...)
}

func Info(format string, args ...interface{}) {
	logf(LevelInfo, format, args...)
}

func Warn(format string, args ...interface{}) {
	logf(LevelWarn, format, args...)
}

func Error(format string, args ...interface{}) {
	logf(LevelError, format, args...)
}

func Fatal(format string, args ...interface{}) {
	logf(LevelError, format, args...)
	os.Exit(1)
}

func Fatalf(format string, args ...interface{}) {
	Fatal(format, args...)
}

func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	std.SetOutput(w)
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slogLevelVar,
	})
	slog.SetDefault(slog.New(handler))
}

func ResetOutput() {
	mu.Lock()
	defer mu.Unlock()
	if logFileHandle != nil {
		_ = logFileHandle.Close()
		logFileHandle = nil
	}
	std.SetOutput(defaultWriter)
	handler := slog.NewTextHandler(defaultWriter, &slog.HandlerOptions{
		Level: slogLevelVar,
	})
	slog.SetDefault(slog.New(handler))
}
