package logging

import (
	"fmt"
	"log"
	"os"
)

// Logger is a structured logger.
type Logger struct {
	logger *log.Logger
}

// NewLogger creates a new Logger instance.
func NewLogger() *Logger {
	return &Logger{
		logger: log.New(os.Stdout, "", log.LstdFlags),
	}
}

// LogError logs an error with a message and context.
func (l *Logger) LogError(message string, err error, context map[string]any, exit bool) {
	logMessage := message
	if err != nil {
		logMessage += ": " + err.Error()
	}
	if context != nil {
		logMessage += " | Context: "
		for key, value := range context {
			logMessage += fmt.Sprintf("%s=%v ", key, value)
		}
	}
	l.logger.Println(logMessage)
	// Do not exit the process here; callers should decide on termination.
}

// LogInfo logs an informational message with context.
func (l *Logger) LogInfo(message string, context map[string]any) {
	logMessage := message
	if context != nil {
		logMessage += " | Context: "
		for key, value := range context {
			logMessage += fmt.Sprintf("%s=%v ", key, value)
		}
	}
	l.logger.Println(logMessage)
}
