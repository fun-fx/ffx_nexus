package evalplugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadFromFile reads a single file's worth of YAML and merges any
// documents into the registry. The empty path is a no-op so the load
// loop doesn't have to special-case absent values.
//
// The returned error is non-nil only for I/O failures; structural
// problems with individual manifests surface as conflicting records
// from Merge so the caller can degrade gracefully (e.g. log and
// continue).
func (r *Registry) LoadFromFile(path string) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// DecodeManyStrict runs Validate per document, so any manifest
	// that opts into `spec.flags: [strict]` emits its unknown-field
	// warnings immediately at boot rather than only on the next
	// admin Save/Patch.
	plugins, err := DecodeManyStrict(raw)
	if err != nil {
		return err
	}
	records := make([]Record, 0, len(plugins))
	for _, p := range plugins {
		records = append(records, Record{
			Plugin:  p,
			Source:  Source{Kind: SourceHelm, Ref: path},
			Enabled: true,
		})
	}
	r.Merge(records)
	return nil
}

// LoadFromDir scans every *.yaml / *.yml file under dir (non-recursive
// to keep Helm-mounted ConfigMaps predictable) and merges them.
//
// The dir must exist; an empty path is treated as "cluster has no
// Helm-installed plugins" and returns nil silently.
func (r *Registry) LoadFromDir(dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		if err := r.LoadFromFile(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
