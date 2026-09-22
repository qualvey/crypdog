// Package logger 提供统一的日志输出抽象，支持级别控制和文件落地。
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
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
	case LevelTrace:
		return slog.Level(-8)
	case LevelDebug:
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
	defaultWriter    io.Writer = os.Stderr
	mu               sync.RWMutex
	currentLevel     Level          = LevelInfo
	currentFormat    string         = "text"
	currentTimestamp                = true
	currentColor                    = false
	slogLevelVar     *slog.LevelVar = new(slog.LevelVar)
	logFileHandle    *os.File
)

type contextKey string

const RequestIDKey contextKey = "request_id"

// WithRequestID 将 Request ID 注入 context
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, RequestIDKey, requestID)
}

// GetRequestID 从 context 获取 Request ID
func GetRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if val, ok := ctx.Value(RequestIDKey).(string); ok {
		return val
	}
	return ""
}

func newSlogHandler(w io.Writer, format string, timestamp bool, color bool) slog.Handler {
	opts := &slog.HandlerOptions{
		Level:     slogLevelVar,
		AddSource: false,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey && !timestamp {
				return slog.Attr{}
			}
			if attr.Key == slog.LevelKey {
				level, ok := attr.Value.Any().(slog.Level)
				if ok {
					attr.Value = slog.StringValue(levelName(level))
				}
			}
			return attr
		},
	}
	if strings.ToLower(strings.TrimSpace(format)) == "json" {
		return slog.NewJSONHandler(w, opts)
	}
	return &textHandler{writer: w, level: slogLevelVar, timestamp: timestamp, color: color, mu: &sync.Mutex{}}
}

// textHandler keeps the human-readable format stable while JSON remains
// available for log collectors. The level is deliberately rendered as a
// visible [LEVEL] token instead of slog's level=LEVEL representation.
type textHandler struct {
	writer    io.Writer
	level     slog.Leveler
	timestamp bool
	color     bool
	attrs     []slog.Attr
	mu        *sync.Mutex
}

func (h *textHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *textHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := append([]slog.Attr(nil), h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})

	component := "app"
	source := "unknown:0"
	var fields []string
	for _, attr := range attrs {
		attr.Value = attr.Value.Resolve()
		switch attr.Key {
		case "component":
			component = attr.Value.String()
		case "source":
			source = attr.Value.String()
		default:
			fields = append(fields, formatAttr(attr))
		}
	}

	var b strings.Builder
	if h.timestamp {
		stamp := record.Time
		if stamp.IsZero() {
			stamp = time.Now()
		}
		b.WriteString(stamp.Format(time.RFC3339Nano))
		b.WriteByte(' ')
	}
	level := "[" + levelName(record.Level) + "]"
	if h.color {
		level = colorizeLevel(record.Level, level)
	}
	b.WriteString(level)
	b.WriteString(" [")
	b.WriteString(component)
	b.WriteByte(' ')
	b.WriteString(source)
	b.WriteString("] ")
	b.WriteString(record.Message)
	for _, field := range fields {
		b.WriteByte(' ')
		b.WriteString(field)
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.writer, b.String())
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *textHandler) WithGroup(_ string) slog.Handler { return h }

func formatAttr(attr slog.Attr) string {
	value := attr.Value.String()
	if attr.Value.Kind() == slog.KindString && (value == "" || strings.ContainsAny(value, " \t\r\n=\"")) {
		value = strconv.Quote(value)
	}
	return attr.Key + "=" + value
}

func colorizeLevel(level slog.Level, text string) string {
	const reset = "\x1b[0m"
	color := "\x1b[37m"
	switch {
	case level >= slog.LevelError:
		color = "\x1b[31m"
	case level >= slog.LevelWarn:
		color = "\x1b[33m"
	case level <= slog.LevelDebug:
		color = "\x1b[36m"
	default:
		color = "\x1b[32m"
	}
	return color + text + reset
}

func levelName(level slog.Level) string {
	switch level {
	case slog.Level(-8):
		return "TRACE"
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelInfo:
		return "INFO"
	case slog.LevelWarn:
		return "WARN"
	case slog.LevelError:
		return "ERROR"
	default:
		return strings.ToUpper(level.String())
	}
}

// callerInfo returns the first application frame outside this package.
func callerInfo() (source, component string) {
	var frames runtime.Frames
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames = *runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if filepath.Base(frame.File) != "logger.go" {
			source = fmtSource(frame.File, frame.Line)
			path := filepath.ToSlash(frame.File)
			if index := strings.Index(path, "/internal/"); index >= 0 {
				rest := path[index+len("/internal/"):]
				component = strings.Split(rest, "/")[0]
			}
			if component == "" {
				component = "app"
			}
			return source, component
		}
		if !more {
			break
		}
	}
	return "unknown:0", "app"
}

func fmtSource(file string, line int) string {
	return filepath.ToSlash(filepath.Base(file)) + ":" + fmt.Sprint(line)
}

func init() {
	slogLevelVar.Set(slog.LevelInfo)
	handler := newSlogHandler(defaultWriter, currentFormat, currentTimestamp, currentColor)
	slog.SetDefault(slog.New(handler))
}

// Init 初始化日志系统（设置日志级别、格式及可选的文件落地）
func Init(level string, format string, output string, outfile string, timestamp ...bool) {
	SetLevel(level)

	mu.Lock()
	defer mu.Unlock()

	if format != "" {
		currentFormat = strings.ToLower(strings.TrimSpace(format))
	}
	currentTimestamp = true
	if len(timestamp) > 0 {
		currentTimestamp = timestamp[0]
	}

	// 清理旧的文件句柄
	if logFileHandle != nil {
		_ = logFileHandle.Close()
		logFileHandle = nil
	}

	var writers []io.Writer

	output = strings.ToLower(strings.TrimSpace(output))
	currentColor = outfile == "" && output != "file"
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

	handler := newSlogHandler(finalWriter, currentFormat, currentTimestamp, currentColor)
	slog.SetDefault(slog.New(handler))
}

// SetLevel 动态调整日志级别（线程安全，同时同步 slog）
func SetLevel(levelText string) {
	mu.Lock()
	defer mu.Unlock()

	parsed := ParseLevel(levelText)
	currentLevel = parsed
	slogLevelVar.Set(parsed.SlogLevel())
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
	logStructured(context.Background(), level, fmt.Sprintf(format, args...))
}

func logStructured(ctx context.Context, level Level, msg string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	source, component := callerInfo()
	attrs := make([]any, 0, len(args)+4)
	attrs = append(attrs, "component", component, "source", source)
	if requestID := GetRequestID(ctx); requestID != "" {
		attrs = append(attrs, "request_id", requestID)
	}
	attrs = append(attrs, args...)
	slog.Log(ctx, level.SlogLevel(), msg, attrs...)
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

// InfoContext 输出带 Context (包含 request_id 等字段) 的结构化日志
func InfoContext(ctx context.Context, msg string, args ...any) {
	if !enabled(LevelInfo) {
		return
	}
	logStructured(ctx, LevelInfo, msg, args...)
}

// WarnContext 输出带 Context 的告警日志
func WarnContext(ctx context.Context, msg string, args ...any) {
	if !enabled(LevelWarn) {
		return
	}
	logStructured(ctx, LevelWarn, msg, args...)
}

// ErrorContext 输出带 Context 的错误日志
func ErrorContext(ctx context.Context, msg string, args ...any) {
	if !enabled(LevelError) {
		return
	}
	logStructured(ctx, LevelError, msg, args...)
}

// DebugContext 输出带 Context 的调试日志
func DebugContext(ctx context.Context, msg string, args ...any) {
	if !enabled(LevelDebug) {
		return
	}
	logStructured(ctx, LevelDebug, msg, args...)
}

// Printf, Println and Fatalf are compatibility helpers for legacy call sites.
// They still produce the same structured output as the context-aware APIs.
func Printf(format string, args ...any) { logf(LevelInfo, format, args...) }

func Println(args ...any) { logf(LevelInfo, "%s", strings.TrimSpace(fmt.Sprintln(args...))) }

func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	handler := newSlogHandler(w, currentFormat, currentTimestamp, false)
	slog.SetDefault(slog.New(handler))
}

func ResetOutput() {
	mu.Lock()
	defer mu.Unlock()
	if logFileHandle != nil {
		_ = logFileHandle.Close()
		logFileHandle = nil
	}
	handler := newSlogHandler(defaultWriter, currentFormat, currentTimestamp, currentColor)
	slog.SetDefault(slog.New(handler))
}
