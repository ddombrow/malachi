package agent

import (
	"encoding/json"
	"strings"
)

// Diagnostic types recorded by providers on a failed assistant message. They
// are what Classify reads, so a provider that records them gets its failures
// classified without anyone matching on error text.
const (
	// DiagnosticHTTPError is a non-2xx response before any stream began.
	// Error.Code holds the HTTP status and Error.Message the response body.
	DiagnosticHTTPError = "http_error"
	// DiagnosticNetworkError is a transport failure: no response, or a stream
	// that broke mid-read. Error.Name is DiagnosticNameTimeout when the
	// failure was a timeout.
	DiagnosticNetworkError = "network_error"
	// DiagnosticStreamError is an error the provider reported inside an
	// otherwise successful stream. Error.Code holds the provider's own error
	// code or type, when it gave one.
	DiagnosticStreamError = "stream_error"
	// DiagnosticMaxTurns marks the message the loop adds when a run reaches
	// LoopConfig.MaxTurns. No provider was called for it.
	DiagnosticMaxTurns = "max_turns"

	// DiagnosticNameTimeout marks a network error that was a timeout.
	DiagnosticNameTimeout = "Timeout"
)

// ErrorKind is why a model call failed, in terms an application can act on:
// tell the user to fix their key, wait and retry, compact, or give up.
type ErrorKind string

const (
	// ErrorNone is a message that did not fail.
	ErrorNone ErrorKind = ""
	// ErrorAborted is a call cancelled through its context. A deadline the
	// caller set is reported this way too; ctx.Err() tells the two apart.
	ErrorAborted ErrorKind = "aborted"
	// ErrorAuth is a rejected API key or a permission the key lacks.
	ErrorAuth ErrorKind = "auth"
	// ErrorQuota is an exhausted balance or plan quota. Unlike a rate limit,
	// waiting does not fix it.
	ErrorQuota ErrorKind = "quota"
	// ErrorRateLimit is too many requests; retrying later may succeed.
	ErrorRateLimit ErrorKind = "rate_limit"
	// ErrorContextLimit is a request too large for the model's context
	// window. Compacting the conversation is the fix.
	ErrorContextLimit ErrorKind = "context_limit"
	// ErrorTimeout is a request that took too long at the provider or on the
	// network.
	ErrorTimeout ErrorKind = "timeout"
	// ErrorNetwork is a transport failure other than a timeout.
	ErrorNetwork ErrorKind = "network"
	// ErrorServer is a provider-side failure: a 5xx, or an overloaded model.
	ErrorServer ErrorKind = "server"
	// ErrorBadRequest is a request the provider refused as invalid. Retrying
	// it unchanged fails again.
	ErrorBadRequest ErrorKind = "bad_request"
	// ErrorMaxTurns is a run stopped by LoopConfig.MaxTurns before the model
	// finished. The model did nothing wrong; it needed more turns.
	ErrorMaxTurns ErrorKind = "max_turns"
	// ErrorUnknown is a failure with nothing to classify it by.
	ErrorUnknown ErrorKind = "unknown"
)

// Retryable reports whether the same request may succeed if sent again later.
func (k ErrorKind) Retryable() bool {
	switch k {
	case ErrorRateLimit, ErrorTimeout, ErrorNetwork, ErrorServer:
		return true
	}
	return false
}

// Classify reports why an assistant message failed, or ErrorNone if it did
// not. It reads the diagnostics providers record, and falls back to the error
// text only when a provider recorded none.
func Classify(m *AssistantMessage) ErrorKind {
	if m == nil {
		return ErrorNone
	}
	switch m.StopReason {
	case StopAborted:
		return ErrorAborted
	case StopError:
	default:
		return ErrorNone
	}
	for _, d := range m.Diagnostics {
		if d.Type == DiagnosticMaxTurns {
			return ErrorMaxTurns
		}
		if d.Error == nil {
			continue
		}
		var kind ErrorKind
		switch d.Type {
		case DiagnosticHTTPError:
			kind = classifyHTTP(statusCode(d.Error.Code), d.Error.Message)
		case DiagnosticNetworkError:
			kind = ErrorNetwork
			if d.Error.Name == DiagnosticNameTimeout {
				kind = ErrorTimeout
			}
		case DiagnosticStreamError:
			code, _ := d.Error.Code.(string)
			kind = classifyText(code + " " + d.Error.Message)
		}
		if kind != "" && kind != ErrorUnknown {
			return kind
		}
	}
	return classifyText(m.ErrorMessage)
}

func classifyHTTP(status int, body string) ErrorKind {
	text := strings.ToLower(body)
	switch {
	case status == 401 || status == 403:
		return ErrorAuth
	case status == 402 || (status == 429 && mentions(text, quotaPhrases)):
		return ErrorQuota
	case status == 429:
		return ErrorRateLimit
	case status == 408 || status == 504:
		return ErrorTimeout
	// Only 400-class rejections are size complaints; a 503 whose body happens
	// to say "too long" is an outage.
	case (status == 400 || status == 413) && mentions(text, contextLimitPhrases):
		return ErrorContextLimit
	case status >= 500:
		return ErrorServer
	case status >= 400:
		return ErrorBadRequest
	}
	return classifyText(body)
}

// classifyText is the fallback for failures that came with words but no
// status: a provider's stream error code, or an error message.
func classifyText(text string) ErrorKind {
	text = strings.ToLower(text)
	switch {
	case strings.TrimSpace(text) == "":
		return ErrorUnknown
	case mentions(text, contextLimitPhrases):
		return ErrorContextLimit
	case mentions(text, quotaPhrases):
		return ErrorQuota
	case mentions(text, []string{"rate limit", "rate_limit", "ratelimit", "too many requests"}):
		return ErrorRateLimit
	case mentions(text, []string{"invalid api key", "invalid_api_key", "authentication", "unauthorized", "permission"}):
		return ErrorAuth
	case mentions(text, []string{"timeout", "timed out", "deadline exceeded"}):
		return ErrorTimeout
	case mentions(text, []string{"overloaded", "server_error", "internal error", "internal server error", "unavailable"}):
		return ErrorServer
	}
	return ErrorUnknown
}

// contextLimitPhrases are how providers say "your request was too big".
var contextLimitPhrases = []string{
	"context length", "context_length", "context window", "maximum context", "too many tokens",
	"too long", "prompt is too long", "reduce the length",
}

var quotaPhrases = []string{"quota", "billing", "insufficient_balance", "insufficient balance", "credit"}

func mentions(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// statusCode reads an HTTP status from a diagnostic, which may be a float64
// decoded from JSON or an int set in this process.
func statusCode(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
