package config

// ChatHistoryConfig configures persistent chat-history capture into PostgreSQL.
//
// This is a fork-local feature (vanbienperu3107/cliproxy_mod). It is deliberately
// kept in its own file, and hooked into the upstream code through the smallest
// possible number of touch points, so that merging upstream stays cheap.
//
// Why a dedicated flag instead of reusing `request-log`:
//
// `request-log` is gated behind `commercial-mode` in two places
// (internal/api/server.go and internal/runtime/executor/helps/logging_helpers.go).
// We keep `commercial-mode: true` on production because it disables a set of
// high-overhead middleware features unrelated to chat history. A separate flag lets
// exactly one of those features back on without dragging the rest along.
type ChatHistoryConfig struct {
	// Enabled turns chat-history capture on. When false the fork behaves exactly
	// like upstream.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// DSN is the PostgreSQL connection string. An empty DSN disables capture even
	// when Enabled is true, so that a misconfigured deployment degrades to
	// "no history" instead of failing to boot.
	DSN string `yaml:"dsn" json:"-"`

	// MaxCaptureBytes caps how many bytes of a single request or response body are
	// held in memory for logging.
	//
	// This cap is enforced in the middleware capture path, NOT in the logger.
	// Upstream's 1 MiB / 32 MiB constants and its disk-spooling path only apply
	// when the logger is disabled (request_logging.go:99 returns early when the
	// logger is enabled), so turning a logger on removes every existing memory
	// guard: captureRequestInfo does an unbounded io.ReadAll plus a decoded copy.
	// A 57 MB body once OOM-killed this container; capping after allocation would
	// not have helped.
	//
	// Zero or negative selects DefaultMaxCaptureBytes.
	MaxCaptureBytes int64 `yaml:"max-capture-bytes" json:"max-capture-bytes"`

	// QueueSize bounds the async write queue. When the queue is full, records are
	// dropped rather than blocking the request path.
	QueueSize int `yaml:"queue-size" json:"queue-size"`

	// RetentionDays is advisory metadata for the cleanup job; the proxy itself
	// never deletes rows.
	RetentionDays int `yaml:"retention-days" json:"retention-days"`

	// Paths restricts capture to these request paths (exact match on the path
	// component). Empty selects DefaultChatHistoryPaths.
	//
	// Without this, upstream's shouldLogRequest only excludes management routes,
	// so every other POST would be persisted together with its headers.
	Paths []string `yaml:"paths" json:"paths"`

	// CaptureUpstream additionally records the request/response exchanged with the
	// upstream provider (after protocol translation, per retry attempt).
	//
	// This re-enables the capture path in logging_helpers.go that commercial-mode
	// switches off. It costs extra memory per request, so it is separate from
	// Enabled and can be turned off on its own.
	CaptureUpstream bool `yaml:"capture-upstream" json:"capture-upstream"`
}

const (
	// DefaultMaxCaptureBytes is the default per-body capture cap (1 MiB).
	DefaultMaxCaptureBytes int64 = 1 << 20

	// DefaultChatHistoryQueueSize is the default async queue depth.
	DefaultChatHistoryQueueSize = 512

	// DefaultChatHistoryRetentionDays is the default retention hint.
	DefaultChatHistoryRetentionDays = 90
)

// DefaultChatHistoryPaths lists the conversational endpoints captured by default.
var DefaultChatHistoryPaths = []string{
	"/v1/chat/completions",
	"/v1/messages",
	"/v1/responses",
}

// Active reports whether chat-history capture should run.
func (c *ChatHistoryConfig) Active() bool {
	return c != nil && c.Enabled && c.DSN != ""
}

// EffectiveMaxCaptureBytes returns the configured cap or the default.
func (c *ChatHistoryConfig) EffectiveMaxCaptureBytes() int64 {
	if c == nil || c.MaxCaptureBytes <= 0 {
		return DefaultMaxCaptureBytes
	}
	return c.MaxCaptureBytes
}

// EffectiveQueueSize returns the configured queue depth or the default.
func (c *ChatHistoryConfig) EffectiveQueueSize() int {
	if c == nil || c.QueueSize <= 0 {
		return DefaultChatHistoryQueueSize
	}
	return c.QueueSize
}

// EffectivePaths returns the configured paths or the default set.
func (c *ChatHistoryConfig) EffectivePaths() []string {
	if c == nil || len(c.Paths) == 0 {
		return DefaultChatHistoryPaths
	}
	return c.Paths
}

// UpstreamCaptureActive reports whether upstream request/response capture is on.
func (c *ChatHistoryConfig) UpstreamCaptureActive() bool {
	return c.Active() && c.CaptureUpstream
}
