package logging

import (
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
func (l *Logger) LogError(message string, err error, context map[string]interface{}) {
	logMessage := message
	if err != nil {
		logMessage += ": " + err.Error()
	}
	if context != nil {
		logMessage += " | Context: "
		for key, value := range context {
			logMessage += key + "=" + value.(string) + " "
		}
	}
	l.logger.Println(logMessage)
}

// LogInfo logs an informational message with context.
func (l *Logger) LogInfo(message string, context map[string]interface{}) {
	logMessage := message
	if context != nil {
		logMessage += " | Context: "
		for key, value := range context {
			// Assuming all values in context are strings for simplicity. Consider type assertion or formatting for other types.
			logMessage += key + "=" + value.(string) + " "
		}
	}
	l.logger.Println(logMessage)
}
