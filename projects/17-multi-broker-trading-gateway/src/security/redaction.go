package security

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

var sensitiveKeyFragments = []string{
	"secret", "token", "password", "passwd", "api_key", "apikey",
	"authorization", "auth_header", "hmac", "private_key", "credential", "bearer",
}

var bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9\-._~+/]+=*`)
var keyValPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|password|hmac)\s*[=:]\s*)([^\s,"'}]+)`)

// LogEntry represents a structured UTC JSON log line.
type LogEntry struct {
	TimestampUTC string         `json:"timestamp_utc"`
	Level        string         `json:"level"`
	Component    string         `json:"component"`
	Message      string         `json:"message"`
	Fields       map[string]any `json:"fields,omitempty"`
}

// RedactingLogger emits structured JSON logs while guaranteeing secrets never appear in output.
type RedactingLogger struct {
	mu            sync.Mutex
	out           io.Writer
	secretManager SecretManager
	extraSecrets  []string
}

// NewRedactingLogger constructs a logger backed by out and optional SecretManager for value scrubbing.
func NewRedactingLogger(out io.Writer, sm SecretManager) *RedactingLogger {
	return &RedactingLogger{
		out:           out,
		secretManager: sm,
	}
}

// RegisterSecret adds a raw secret literal to the active redaction scrub list.
func (l *RedactingLogger) RegisterSecret(raw string) {
	if raw == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.extraSecrets = append(l.extraSecrets, raw)
}

// Info logs an informational event.
func (l *RedactingLogger) Info(component, msg string, fields map[string]any) {
	l.log("INFO", component, msg, fields)
}

// Warn logs a warning event.
func (l *RedactingLogger) Warn(component, msg string, fields map[string]any) {
	l.log("WARN", component, msg, fields)
}

// Error logs an error event.
func (l *RedactingLogger) Error(component, msg string, fields map[string]any) {
	l.log("ERROR", component, msg, fields)
}

func (l *RedactingLogger) log(level, component, msg string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	knownSecrets := append([]string{}, l.extraSecrets...)
	if l.secretManager != nil {
		knownSecrets = append(knownSecrets, l.secretManager.RegisteredSecretValues()...)
	}

	sanitizedFields := make(map[string]any, len(fields))
	for k, v := range fields {
		if isSensitiveKey(k) {
			sanitizedFields[k] = "[REDACTED]"
			continue
		}
		sanitizedFields[k] = l.scrubValue(v, knownSecrets)
	}

	entry := LogEntry{
		TimestampUTC: time.Now().UTC().Format(time.RFC3339Nano),
		Level:        level,
		Component:    component,
		Message:      scrubString(msg, knownSecrets),
		Fields:       sanitizedFields,
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line := scrubString(string(b), knownSecrets)
	_, _ = fmt.Fprintln(l.out, line)
}

func (l *RedactingLogger) scrubValue(v any, secrets []string) any {
	switch val := v.(type) {
	case SecretString:
		return "[REDACTED]"
	case string:
		return scrubString(val, secrets)
	case error:
		return scrubString(val.Error(), secrets)
	case map[string]any:
		cp := make(map[string]any, len(val))
		for k, mv := range val {
			if isSensitiveKey(k) {
				cp[k] = "[REDACTED]"
			} else {
				cp[k] = l.scrubValue(mv, secrets)
			}
		}
		return cp
	default:
		return v
	}
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, frag := range sensitiveKeyFragments {
		if strings.Contains(lk, frag) {
			return true
		}
	}
	return false
}

func scrubString(s string, secrets []string) string {
	out := s
	for _, sec := range secrets {
		if len(sec) >= 4 {
			out = strings.ReplaceAll(out, sec, "[REDACTED]")
		}
	}
	out = bearerPattern.ReplaceAllString(out, "${1}[REDACTED]")
	out = keyValPattern.ReplaceAllString(out, "${1}[REDACTED]")
	return out
}
