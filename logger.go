package main

import "log"

type customLogger struct {
	logger *log.Logger
	prefix string
}

func newCustomLogger(baseLogger any, additionalPrefix string) *customLogger {
	switch logger := baseLogger.(type) {
	case *log.Logger:
		return &customLogger{
			logger: logger,
			prefix: additionalPrefix,
		}
	case *customLogger:
		return &customLogger{
			logger: logger.logger,
			prefix: logger.prefix + additionalPrefix,
		}
	default:
		panic("unsupported logger type")
	}
}

func (cl *customLogger) Println(v ...any) {
	cl.logger.Println(cl.prefix, v)
}

func (cl *customLogger) Printf(format string, v ...any) {
	cl.logger.Printf(cl.prefix+format, v...)
}
