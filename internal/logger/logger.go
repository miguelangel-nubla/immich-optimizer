package logger

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

type Level = slog.Level

const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

type Logger struct {
	logger *log.Logger
	level  *slog.LevelVar
	prefix string
}

func New(baseLog *log.Logger, prefix string) *Logger {
	return NewWithLevel(baseLog, "info", prefix)
}

func NewWithLevel(baseLog *log.Logger, levelStr string, prefix string) *Logger {
	lvlVar := new(slog.LevelVar)
	lvlVar.Set(ParseLevel(levelStr))

	if baseLog == nil {
		baseLog = log.New(os.Stdout, "", log.Ldate|log.Ltime)
	}

	return &Logger{
		logger: baseLog,
		level:  lvlVar,
		prefix: prefix,
	}
}

func ParseLevel(s string) Level {
	var l Level
	if err := l.UnmarshalText([]byte(strings.ToLower(strings.TrimSpace(s)))); err != nil {
		return LevelInfo
	}
	return l
}

func (cl *Logger) SetLevel(levelStr string) {
	if cl != nil && cl.level != nil {
		cl.level.Set(ParseLevel(levelStr))
	}
}

func (cl *Logger) WithPrefix(additionalPrefix string) *Logger {
	if cl == nil {
		return nil
	}
	return &Logger{
		logger: cl.logger,
		level:  cl.level,
		prefix: cl.prefix + additionalPrefix,
	}
}

func (cl *Logger) enabled(lvl Level) bool {
	if cl == nil || cl.logger == nil {
		return false
	}
	if cl.level == nil {
		return true
	}
	return cl.level.Level() <= lvl
}

func (cl *Logger) Println(v ...any) {
	if !cl.enabled(LevelInfo) {
		return
	}
	if cl.prefix == "" {
		cl.logger.Println(v...)
	} else {
		args := append([]any{cl.prefix}, v...)
		cl.logger.Println(args...)
	}
}

func (cl *Logger) Printf(format string, v ...any) {
	if !cl.enabled(LevelInfo) {
		return
	}
	cl.logger.Printf(cl.prefix+format, v...)
}

func (cl *Logger) Debugf(format string, v ...any) {
	if !cl.enabled(LevelDebug) {
		return
	}
	cl.logger.Printf(cl.prefix+"[DEBUG] "+format, v...)
}

func (cl *Logger) Infof(format string, v ...any) {
	if !cl.enabled(LevelInfo) {
		return
	}
	cl.logger.Printf(cl.prefix+format, v...)
}

func (cl *Logger) Warnf(format string, v ...any) {
	if !cl.enabled(LevelWarn) {
		return
	}
	cl.logger.Printf(cl.prefix+"[WARN] "+format, v...)
}

func (cl *Logger) Errorf(format string, v ...any) {
	if !cl.enabled(LevelError) {
		return
	}
	cl.logger.Printf(cl.prefix+"[ERROR] "+format, v...)
}
