package mcp

import (
	"errors"
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// ServerSpec is the YAML manifest stored in mcp_servers.spec_yaml.
type ServerSpec struct {
	Connection ConnectionSpec `yaml:"connection"`
	// AllowedVirtualKeyIDs restricts which virtual keys may call this server.
	// Empty means all keys in the org may use it.
	AllowedVirtualKeyIDs []string `yaml:"allowed_virtual_key_ids,omitempty"`
	TimeoutMs            int      `yaml:"timeout_ms,omitempty"`
	Enabled              *bool    `yaml:"enabled,omitempty"`
	// StickyHTTP keeps a persistent HTTP connection (default true).
	StickyHTTP *bool `yaml:"sticky_http,omitempty"`
}

// ConnectionSpec describes how to reach an upstream MCP server.
type ConnectionSpec struct {
	Type    string            `yaml:"type"` // stdio | http
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

var (
	ErrInvalidSpec = errors.New("mcp: invalid server spec")
)

// DecodeSpec parses and validates spec YAML.
func DecodeSpec(raw []byte) (*ServerSpec, error) {
	var spec ServerSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("decode mcp spec: %w", err)
	}
	if err := ValidateSpec(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// ValidateSpec checks a decoded spec.
func ValidateSpec(spec *ServerSpec) error {
	if spec == nil {
		return fmt.Errorf("%w: nil spec", ErrInvalidSpec)
	}
	typ := strings.ToLower(strings.TrimSpace(spec.Connection.Type))
	switch typ {
	case "stdio":
		if strings.TrimSpace(spec.Connection.Command) == "" {
			return fmt.Errorf("%w: stdio connection requires command", ErrInvalidSpec)
		}
	case "http":
		if strings.TrimSpace(spec.Connection.URL) == "" {
			return fmt.Errorf("%w: http connection requires url", ErrInvalidSpec)
		}
	default:
		return fmt.Errorf("%w: connection.type must be stdio or http", ErrInvalidSpec)
	}
	if spec.TimeoutMs < 0 {
		return fmt.Errorf("%w: timeout_ms must be non-negative", ErrInvalidSpec)
	}
	return nil
}

// ConnectionType returns the normalized connection type.
func (s *ServerSpec) ConnectionType() string {
	return strings.ToLower(strings.TrimSpace(s.Connection.Type))
}

// Timeout returns the configured timeout or a default.
func (s *ServerSpec) Timeout() int {
	if s.TimeoutMs > 0 {
		return s.TimeoutMs
	}
	return 60_000
}

// Sticky returns whether HTTP connections should be sticky.
func (s *ServerSpec) Sticky() bool {
	if s.StickyHTTP == nil {
		return true
	}
	return *s.StickyHTTP
}

// AllowsVirtualKey reports whether the given virtual key may call this server.
func (s *ServerSpec) AllowsVirtualKey(vkeyID string) bool {
	if len(s.AllowedVirtualKeyIDs) == 0 {
		return true
	}
	for _, id := range s.AllowedVirtualKeyIDs {
		if id == vkeyID {
			return true
		}
	}
	return false
}
