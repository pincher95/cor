package errorhandling

import (
	"os"

	"github.com/pincher95/cor/pkg/handlers/logging"
)

// ErrorHandler handles errors in a consistent manner.
type ErrorHandler struct {
	logger *logging.Logger
}

// NewErrorHandler creates a new ErrorHandler instance.
func NewErrorHandler(logger *logging.Logger) *ErrorHandler {
	return &ErrorHandler{logger: logger}
}

// HandleError handles an error by logging it and optionally exiting the application.
func (eh *ErrorHandler) HandleError(message string, err error, context map[string]interface{}, exit bool) {
	eh.logger.LogError(message, err, context)
	if exit {
		os.Exit(1)
	}
}
