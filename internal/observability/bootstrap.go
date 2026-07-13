package observability

import (
	"context"
	"errors"
	"log/slog"
)

// Bootstrapper lets a subsystem wire up an external tool on startup (one-shot,
// idempotent). It runs out-of-band from the gateway data path: a failure here
// must never delay request handling. Mirrors the Recorder fan-out pattern used
// by MultiRecorder for clickhouse / otlp / noop so the nextus adapter set
// stays consistent across "hot path" and "boot path" integrations.
//
// Conventions (same as Recorder / MultiRecorder):
//   - Constructors return nil when disabled (callers append to Multi only if
//     non-nil). Empty config == opt-out.
//   - Bootstrap is called once on boot; subsequent calls are safe and idempotent.
//   - All errors are returned to the caller; main.go downgrades them to slog.Warn
//     so a faulty Metabase or Grafana does not gate Nexus startup.
type Bootstrapper interface {
	Name() string
	// Bootstrap performs the one-shot setup. Must be idempotent so retrying
	// after a recovered transient failure is safe.
	Bootstrap(ctx context.Context) error
}

// MultiBootstrapper fans a single Bootstrap call out to a list of child
// bootstrappers. Errors are aggregated with errors.Join; the first failure
// does NOT short-circuit the others so each subsystem gets a chance to wire
// itself up even when its peer is down. Nil entries are ignored (matching
// MultiRecorder).
type MultiBootstrapper struct {
	children []Bootstrapper
	log      *slog.Logger
}

// NewMultiBootstrapper composes bootstrapper implementations. Nil children
// are skipped.
func NewMultiBootstrapper(children ...Bootstrapper) *MultiBootstrapper {
	out := make([]Bootstrapper, 0, len(children))
	for _, c := range children {
		if c != nil {
			out = append(out, c)
		}
	}
	return &MultiBootstrapper{children: out}
}

// SetLogger attaches a logger so partially-failed bootstraps can leave a breadcrumb.
// Optional; nil is fine.
func (m *MultiBootstrapper) SetLogger(log *slog.Logger) { m.log = log }

// Bootstrap returns a joined error covering every failed child. Each child
// is invoked sequentially so the log output stays readable when ops folks
// tail the boot sequence. A nil receiver is a no-op.
func (m *MultiBootstrapper) Bootstrap(ctx context.Context) error {
	if m == nil || len(m.children) == 0 {
		return nil
	}
	var errs []error
	for _, c := range m.children {
		if err := c.Bootstrap(ctx); err != nil {
			if m.log != nil {
				m.log.Warn("bootstrap child failed", "name", c.Name(), "err", err)
			}
			errs = append(errs, err)
			continue
		}
		if m.log != nil {
			m.log.Info("bootstrap child ok", "name", c.Name())
		}
	}
	return errors.Join(errs...)
}

// Names returns the registered child names (without invoking them). Useful
// for logging the planned bootstrap set on startup.
func (m *MultiBootstrapper) Names() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.children))
	for i, c := range m.children {
		out[i] = c.Name()
	}
	return out
}
