package config

import (
	"log/slog"
	"net/url"
)

// Secret is a string that is never shown by accident: fmt (%v, %+v, %s, %#v),
// slog and encoding/json all print a placeholder. Reveal returns the value.
type Secret string

const redacted = "[REDACTED]"

func (s Secret) Reveal() string { return string(s) }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue shows a URL with its password removed, so the startup log still
// says which host and database are used. Anything that is not a URL is hidden.
func (s Secret) LogValue() slog.Value {
	if u, err := url.Parse(string(s)); err == nil && u.Scheme != "" {
		return slog.StringValue(redactURL(string(s)))
	}
	return slog.StringValue(redacted)
}
