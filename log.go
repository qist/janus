package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

type Logger struct {
	mu    sync.Mutex
	level Level
	out   *os.File
}

func NewLogger(s string) *Logger {
	l := &Logger{level: LevelInfo, out: os.Stdout}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace":
		l.level = LevelDebug
	case "info", "":
		l.level = LevelInfo
	case "warn", "warning":
		l.level = LevelWarn
	case "error", "fatal":
		l.level = LevelError
	}
	return l
}

func (l *Logger) emit(lv Level, tag, format string, args ...any) {
	if lv < l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %-5s %s\n",
		time.Now().Format("2006-01-02T15:04:05.000Z"), tag, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.WriteString(line)
}

func (l *Logger) Debugf(format string, args ...any) { l.emit(LevelDebug, "DEBUG", format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.emit(LevelInfo, "INFO", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.emit(LevelWarn, "WARN", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.emit(LevelError, "ERROR", format, args...) }

// Redact 掩掉任何形如 Bearer xxx / Basic xxx 的凭据。
func Redact(s string) string {
	for _, p := range []string{"Bearer ", "Basic "} {
		if i := strings.Index(s, p); i >= 0 {
			return s[:i+len(p)] + "***"
		}
	}
	return s
}
