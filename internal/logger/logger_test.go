package logger

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestLoggerPrefixAndOutput(t *testing.T) {
	var buf bytes.Buffer
	baseLog := log.New(&buf, "", 0)

	l := New(baseLog, "[TEST] ")

	l.Println("hello world")
	if !strings.Contains(buf.String(), "[TEST]") || !strings.Contains(buf.String(), "hello world") {
		t.Errorf("expected '[TEST]' and 'hello world', got %q", buf.String())
	}

	buf.Reset()
	l.Printf("foo %s", "bar")
	if !strings.Contains(buf.String(), "[TEST] foo bar") {
		t.Errorf("expected '[TEST] foo bar', got %q", buf.String())
	}

	buf.Reset()
	subL := l.WithPrefix("[SUB] ")
	subL.Printf("nested %d", 42)
	if !strings.Contains(buf.String(), "[TEST] [SUB] nested 42") {
		t.Errorf("expected '[TEST] [SUB] nested 42', got %q", buf.String())
	}
}

func TestNilLoggerHandling(t *testing.T) {
	var nilLog *Logger
	nilLog.Println("should not panic")
	nilLog.Printf("should not panic %s", "arg")
	nilLog.Debugf("should not panic")
	nilLog.Infof("should not panic")
	nilLog.Warnf("should not panic")
	nilLog.Errorf("should not panic")

	sub := nilLog.WithPrefix("prefix")
	if sub != nil {
		t.Errorf("expected WithPrefix on nil logger to return nil")
	}

	emptyLog := New(nil, "prefix")
	emptyLog.Println("should not panic")
	emptyLog.Printf("should not panic")
}

func TestLogLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	baseLog := log.New(&buf, "", 0)

	l := NewWithLevel(baseLog, "warn", "[TEST] ")

	l.Debugf("debug msg")
	l.Infof("info msg")
	if buf.Len() > 0 {
		t.Errorf("expected debug and info to be filtered out at 'warn' level, got %q", buf.String())
	}

	l.Warnf("warn msg")
	if !strings.Contains(buf.String(), "[WARN] warn msg") {
		t.Errorf("expected warn msg in log, got %q", buf.String())
	}

	buf.Reset()
	l.Errorf("error msg")
	if !strings.Contains(buf.String(), "[ERROR] error msg") {
		t.Errorf("expected error msg in log, got %q", buf.String())
	}

	buf.Reset()
	l.SetLevel("debug")
	l.Debugf("debug msg 2")
	if !strings.Contains(buf.String(), "[DEBUG] debug msg 2") {
		t.Errorf("expected debug msg after dynamically setting level to debug, got %q", buf.String())
	}
}
