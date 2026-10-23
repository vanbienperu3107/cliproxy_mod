package dsweb

import (
	"fmt"
	"net/http"
	"time"
)

// HTTPError is a failure with a status the host can surface to the client. The
// plugin ABI carries this through the envelope's http_status field.
type HTTPError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *HTTPError) StatusCode() int {
	if e == nil || e.Status == 0 {
		return http.StatusBadGateway
	}
	return e.Status
}

func (e *HTTPError) RetryAfterSeconds() int {
	if e == nil || e.RetryAfter <= 0 {
		return 0
	}
	return int(e.RetryAfter.Round(time.Second).Seconds())
}

func newHTTPError(status int, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Message: fmt.Sprintf(format, args...)}
}
