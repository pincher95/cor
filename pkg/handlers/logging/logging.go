/*
Copyright 2024 Elastic Scaler Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
