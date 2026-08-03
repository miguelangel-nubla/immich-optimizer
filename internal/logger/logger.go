package logger

import "log"

type Logger struct {
	logger *log.Logger
	prefix string
}

func New(baseLogger *log.Logger, prefix string) *Logger {
	return &Logger{
		logger: baseLogger,
		prefix: prefix,
	}
}

func (cl *Logger) WithPrefix(additionalPrefix string) *Logger {
	if cl == nil {
		return nil
	}
	return &Logger{
		logger: cl.logger,
		prefix: cl.prefix + additionalPrefix,
	}
}

func (cl *Logger) Println(v ...any) {
	if cl == nil || cl.logger == nil {
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
	if cl == nil || cl.logger == nil {
		return
	}
	cl.logger.Printf(cl.prefix+format, v...)
}
