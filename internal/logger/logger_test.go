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

	sub := nilLog.WithPrefix("prefix")
	if sub != nil {
		t.Errorf("expected WithPrefix on nil logger to return nil")
	}

	emptyLog := New(nil, "prefix")
	emptyLog.Println("should not panic")
	emptyLog.Printf("should not panic")
}
