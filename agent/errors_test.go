package agent

import "testing"

func TestClassify(t *testing.T) {
	failed := func(text string, diags ...Diagnostic) *AssistantMessage {
		m := NewAssistantMessage("m")
		m.StopReason, m.ErrorMessage, m.Diagnostics = StopError, text, diags
		return m
	}
	http := func(status any, body string) Diagnostic {
		return Diagnostic{Type: DiagnosticHTTPError, Error: &DiagnosticError{Message: body, Code: status}}
	}
	stream := func(code any, text string) Diagnostic {
		return Diagnostic{Type: DiagnosticStreamError, Error: &DiagnosticError{Message: text, Code: code}}
	}
	aborted := NewAssistantMessage("m")
	aborted.StopReason = StopAborted

	cases := []struct {
		name string
		m    *AssistantMessage
		want ErrorKind
	}{
		{"success", NewAssistantMessage("m"), ErrorNone},
		{"nil", nil, ErrorNone},
		{"aborted", aborted, ErrorAborted},
		{"rejected key", failed("HTTP 401", http(401, `{"error":{"message":"invalid api key"}}`)), ErrorAuth},
		{"forbidden", failed("HTTP 403", http(403, "nope")), ErrorAuth},
		{"rate limited", failed("HTTP 429", http(429, "slow down")), ErrorRateLimit},
		{"quota on 429", failed("HTTP 429", http(429, `{"error":{"code":"insufficient_quota"}}`)), ErrorQuota},
		{"payment required", failed("HTTP 402", http(402, "")), ErrorQuota},
		{"prompt too long", failed("HTTP 400", http(400, "prompt is too long: 300000 tokens > 200000")), ErrorContextLimit},
		{"other bad request", failed("HTTP 400", http(400, "unknown field")), ErrorBadRequest},
		// A 503 whose body happens to say "too long" is an outage.
		{"outage mentioning length", failed("HTTP 503", http(503, "upstream request took too long")), ErrorServer},
		{"gateway timeout", failed("HTTP 504", http(504, "")), ErrorTimeout},
		{"status decoded from JSON", failed("HTTP 429", http(float64(429), "")), ErrorRateLimit},
		{"network timeout", failed("dial", Diagnostic{Type: DiagnosticNetworkError,
			Error: &DiagnosticError{Name: DiagnosticNameTimeout, Message: "i/o timeout"}}), ErrorTimeout},
		{"connection reset", failed("stream read", Diagnostic{Type: DiagnosticNetworkError,
			Error: &DiagnosticError{Name: "NetworkError", Message: "connection reset by peer"}}), ErrorNetwork},
		{"stream rate limit code", failed("Provider stream error", stream("rate_limit_exceeded", "slow down")), ErrorRateLimit},
		{"stream context code", failed("Provider stream error", stream("context_length_exceeded", "")), ErrorContextLimit},
		{"stream overloaded type", failed("Provider stream error", stream("overloaded_error", "Overloaded")), ErrorServer},
		{"no diagnostics, size in text", failed("maximum context length is 128000 tokens"), ErrorContextLimit},
		{"nothing to go on", failed("something broke"), ErrorUnknown},
	}
	for _, tc := range cases {
		if got := Classify(tc.m); got != tc.want {
			t.Errorf("%s: Classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestErrorKindRetryable(t *testing.T) {
	for kind, want := range map[ErrorKind]bool{
		ErrorRateLimit: true, ErrorServer: true, ErrorTimeout: true, ErrorNetwork: true,
		ErrorAuth: false, ErrorQuota: false, ErrorContextLimit: false, ErrorBadRequest: false,
		ErrorAborted: false, ErrorUnknown: false, ErrorNone: false,
	} {
		if kind.Retryable() != want {
			t.Errorf("%q.Retryable() = %v, want %v", kind, !want, want)
		}
	}
}
