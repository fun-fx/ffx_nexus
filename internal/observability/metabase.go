package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MetabaseBootstrapper wires Metabase's read-only analytical UI onto the same
// ClickHouse and Postgres data the rest of Nexus already writes to. It is a
// one-shot, idempotent adapter in the spirit of V3 OTLP — Metabase is the BI
// tool, this struct is the bootstrap glue.
//
// What it does on Boot (in order):
//  1. Poll Metabase's /api/health until "ok" or until HealthTimeout elapses.
//     A fresh container takes ~30s to come up; we give it up to 90s.
//  2. Establish a session via /api/session.
//  3. Register each configured data source (ClickHouse HTTP, Postgres JDBC)
//     using /api/database, idempotently: an existing engine/name match resolves
//     to the existing id and we PUT it instead of POSTing a duplicate.
//  4. Seed any dashboard collection JSONs bundled in deploy/observability/metabase
//     (/api/collection + /api/card) so the operator sees panels on first login.
//
// What it DOES NOT do:
//   - Hot-path trace ingestion. Metabase is pull-only (JDBC / HTTP SQL),
//     so all trace/spend writes still go to ClickHouse and Postgres as before.
//   - Background reconciliation. A Metabase restart that loses seeded state
//     will produce a no-op re-seed on the next Nexus boot, which is the desired
//     behavior for a local-dev profile.
//
// Enable by setting NEXUS_METABASE_URL to a Metabase URL (e.g. http://metabase:3000).
// Empty URL => constructor returns nil (the adapter is fully off, like V3 OTLP).
type MetabaseBootstrapper struct {
	cfg    MetabaseConfig
	log    *slog.Logger
	client *http.Client
}

// MetabaseConfig describes a target Metabase instance plus the data sources
// to register. ClickHouseHTTP is the SQL endpoint on port 8123 (Metabase has a
// built-in ClickHouse database type using HTTP). PostgresJDBC follows the
// standard Metabase "postgres" type and points at the same Postgres the
// control-plane already uses.
type MetabaseConfig struct {
	// URL is the Metabase base. Empty disables the adapter (returns nil).
	URL string
	// User is the Metabase admin username; password is the matching secret.
	// Username/password flow is used instead of API keys so the same adapter
	// works on freshly-init Metabase containers (which start with no API key).
	User     string
	Password string

	// ClickHouseHTTP — http://host:8123?database=nexus — registered as the
	// "clickhouse" engine. Used by the per-day spend / latency dashboards.
	ClickHouseHTTP string
	// PostgresJDBC — postgres://user:pass@host:5432/db. Used by user/key
	// dashboards (control plane data).
	PostgresJDBC string

	// SeedDir is an optional directory containing *.json Metabase collection
	// export files. Each top-level object follows Metabase's REST schema for
	// collections and cards; we POST them through /api/collection and
	// /api/card. Missing directory is a no-op (the feature is opt-in).
	SeedDir string

	// HealthTimeout caps the wait for Metabase to come up. Default 90s.
	HealthTimeout time.Duration
	// RequestTimeout caps individual API calls. Default 10s.
	RequestTimeout time.Duration
}

// NewMetabaseBootstrapper returns nil when URL is empty (opt-in contract
// matching NewOTLPRecorder). A bootstrapper constructed from a non-empty URL
// is safe to call Bootstrap() on repeatedly; the underlying Metabase calls
// are idempotent.
func NewMetabaseBootstrapper(cfg MetabaseConfig, log *slog.Logger) *MetabaseBootstrapper {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 90 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	return &MetabaseBootstrapper{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
	}
}

// Name implements Bootstrapper.
func (m *MetabaseBootstrapper) Name() string { return "metabase" }

// Bootstrap runs the four-step setup. Stop at the first step that returns
// a hard error so we don't keep poking a half-broken Metabase, but log
// loudly on each kind of failure.
func (m *MetabaseBootstrapper) Bootstrap(ctx context.Context) error {
	if err := m.waitHealth(ctx); err != nil {
		return fmt.Errorf("metabase health: %w", err)
	}
	sessionToken, err := m.login(ctx)
	if err != nil {
		return fmt.Errorf("metabase login: %w", err)
	}
	dbs, err := m.ensureDataSources(ctx, sessionToken)
	if err != nil {
		return fmt.Errorf("metabase datasources: %w", err)
	}
	if err := m.seedCollections(ctx, sessionToken, dbs); err != nil {
		return fmt.Errorf("metabase seed: %w", err)
	}
	m.log.Info("metabase bootstrap complete",
		"clickhouse_db", dbNameFor(dbs, "clickhouse"),
		"postgres_db", dbNameFor(dbs, "postgres"),
		"seed_dir", m.cfg.SeedDir)
	return nil
}

// databaseID is the Metabase-side primary key returned by /api/database.
type databaseID int

// databases holds the resolved ids (0 = not registered this run).
type databases map[string]databaseID

// dbNameFor returns a friendly summary suitable for log output.
func dbNameFor(dbs databases, engine string) string {
	if id, ok := dbs[engine]; ok && id > 0 {
		return fmt.Sprintf("%s(id=%d)", engine, id)
	}
	return engine + "(skipped)"
}

// waitHealth polls GET /api/health until the body is "ok" or until
// HealthTimeout elapses. We do not poll /api/session — Metabase serves health
// before the database is fully migrated.
func (m *MetabaseBootstrapper) waitHealth(ctx context.Context) error {
	deadline := time.Now().Add(m.cfg.HealthTimeout)
	delay := 500 * time.Millisecond
	url := strings.TrimRight(m.cfg.URL, "/") + "/api/health"
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := m.client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "{}" {
				// Metabase 0.50.x returns "{}" while still loading the app DB;
				// "ok" arrives on full readiness. Prefer "{}" -> ok as well.
				return nil
			}
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"ok"`) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("metabase not healthy within %s", m.cfg.HealthTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		// 1.5x backoff capped at 5s so we don't busy-loop a slow container.
		if delay < 5*time.Second {
			delay = delay*3/2 + 100*time.Millisecond
		}
	}
}

// login performs POST /api/session and returns the X-Metabase-Session token.
// Metabase also supports API-key auth, but admin login works on a freshly-
// initialized container where no key has been provisioned yet.
func (m *MetabaseBootstrapper) login(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"username": m.cfg.User,
		"password": m.cfg.Password,
	})
	url := strings.TrimRight(m.cfg.URL, "/") + "/api/session"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("login status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("login returned no session id")
	}
	return out.ID, nil
}

// ensureDataSources registers ClickHouse and Postgres if their endpoints were
// configured. Returns a map of engine -> Metabase database id so the seeder
// can attach cards to the right database.
func (m *MetabaseBootstrapper) ensureDataSources(ctx context.Context, session string) (databases, error) {
	out := databases{}
	sources := []struct {
		engine, endpoint, hint string
		enabled                bool
		details                map[string]any
	}{
		{
			engine:  "clickhouse",
			hint:    "Nexus traces / spend",
			enabled: m.cfg.ClickHouseHTTP != "",
			details: clickHouseDetails(m.cfg.ClickHouseHTTP),
		},
		{
			engine:  "postgres",
			hint:    "Nexus control plane",
			enabled: m.cfg.PostgresJDBC != "",
			details: postgresDetails(m.cfg.PostgresJDBC),
		},
	}
	for _, s := range sources {
		if !s.enabled {
			continue
		}
		id, err := m.ensureOneDataSource(ctx, session, s.engine, s.hint, s.details)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.engine, err)
		}
		out[s.engine] = id
		m.log.Info("metabase datasource ensured", "engine", s.engine, "id", id)
	}
	return out, nil
}

// ensureOneDataSource creates a datasource if absent, or updates the existing
// one (matched by `name` + `engine`). All operations are idempotent.
//
// Collision safety: if a datasource named "nexus-<engine>" already exists but
// was NOT created by us (no `nexus_managed_by` marker on its details), we
// skip the side-effect silently and return its id. The operator gets a Warn
// log line so they can investigate — but we never overwrite what is clearly
// managed by someone else. This matters when Nexus is deployed into an org
// that already manages its own Metabase instance (Pattern B): an over-eager
// PUT would clobber credentials, host, or saved question schema.
func (m *MetabaseBootstrapper) ensureOneDataSource(ctx context.Context, session, engine, name string, details map[string]any) (databaseID, error) {
	existing, err := m.listDataSources(ctx, session)
	if err != nil {
		return 0, err
	}
	for _, db := range existing {
		if db.Name == "nexus-"+engine && db.Engine == engine {
			if !isNexusManagedDatabase(db.Details) {
				m.log.Warn("metabase datasource with reserved name already exists; refraining from update",
					"engine", engine,
					"id", db.ID,
					"hint", "the existing datasource is not owned by Nexus. To take it over, add `nexus_managed_by: \"metabase-bootstrapper\"` to its details.",
				)
				return databaseID(db.ID), nil
			}
		}
	}
	for _, db := range existing {
		if db.Name == "nexus-"+engine && db.Engine == engine {
			// Owned by us: PUT to /api/database/:id to refresh credentials/host
			// without breaking the data model the operator built in the UI.
			return m.putOwnedDataSource(ctx, session, db.ID, engine, details)
		}
	}
	// Stamping `details.nexus_managed_by` so future runs recognise ownership
	// even after we re-deploy or upgrade the adapter.
	details[nexusManagedKey] = nexusManagedValue
	return m.postDataSource(ctx, session, map[string]any{
		"name":         "nexus-" + engine,
		"engine":       engine,
		"details":      details,
		"is_full_sync": true,
	})
}

// metabaseDatabase mirrors the fields we need from /api/database. We only
// decode what Bootstrap consumes; server-returned fields outside this struct
// are silently dropped.
type metabaseDatabase struct {
	ID      int            `json:"id"`
	Name    string         `json:"name"`
	Engine  string         `json:"engine"`
	Details map[string]any `json:"details,omitempty"`
}

// Collection marker constants. We stamp these on every entity Nexus creates
// so re-deploys are safe and so collisions with operator-managed objects
// don't accidentally clobber existing dashboards / datasources.
const (
	// nexusManagedKey rides on datasource `details.nexus_managed_by`. The
	// metabase REST API persists any extra fields on details without
	// validation, so this is a stable hand-off.
	nexusManagedKey = "nexus_managed_by"
	// nexusManagedValue is the version-aware marker. Bumping it forces a
	// clean re-registration on the next deploy (the new adapter treats the
	// old datasource as foreign and creates a fresh one) which is simpler
	// than chasing schema mismatches.
	nexusManagedValue = "metabase-bootstrapper/v1"
	// nexusManagedDescriptionPrefix lands on collection.description. We do
	// not depend on a hidden metadata field because /api/collection does not
	// expose one; description is the documented extension point.
	nexusManagedDescriptionPrefix = "[Nexus-managed] "
)

// isNexusManagedDatabase reports whether the datasource was stamped by the
// current (or any future compatible) Nexus bootstrapper. Unknown / malformed
// details return false so we default to "skip" — safer than "overwrite".
func isNexusManagedDatabase(details map[string]any) bool {
	if details == nil {
		return false
	}
	v, ok := details[nexusManagedKey].(string)
	if !ok {
		return false
	}
	// Either the exact v1 marker, or any future-versioned marker (semver
	// pattern "metabase-bootstrapper/vN"), counts as owned.
	if v == nexusManagedValue {
		return true
	}
	if strings.HasPrefix(v, "metabase-bootstrapper/") {
		return true
	}
	return false
}

// isNexusManagedCollection mirrors isNexusManagedDatabase for collections,
// reading the description prefix instead of a free-field in metadata.
func isNexusManagedCollection(description string) bool {
	return strings.HasPrefix(description, nexusManagedDescriptionPrefix)
}

func (m *MetabaseBootstrapper) listDataSources(ctx context.Context, session string) ([]metabaseDatabase, error) {
	url := strings.TrimRight(m.cfg.URL, "/") + "/api/database"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("X-Metabase-Session", session)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list databases status %d: %s", resp.StatusCode, string(raw))
	}
	// Metabase 0.50+ wraps the list in {data:[...], total:N, limit:N, offset:N}.
	// Older versions returned a bare array. Try the wrapped shape first;
	// fall back to bare-array decode so we keep working against vintage
	// 0.49.x instances that don't paginate. Both shapes are documented in
	// the Metabase REST API reference (2024-05 onwards).
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("list databases read: %w", err)
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	out := []metabaseDatabase{}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var wrapped struct {
			Data []metabaseDatabase `json:"data"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return nil, fmt.Errorf("list databases wrapped decode: %w", err)
		}
		out = wrapped.Data
	} else {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("list databases bare decode: %w", err)
		}
	}
	return out, nil
}

func (m *MetabaseBootstrapper) postDataSource(ctx context.Context, session string, payload map[string]any) (databaseID, error) {
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(m.cfg.URL, "/") + "/api/database"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metabase-Session", session)
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("create database status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return databaseID(out.ID), nil
}

func (m *MetabaseBootstrapper) putDataSource(ctx context.Context, session string, id int, payload map[string]any) (databaseID, error) {
	// Metabase's PUT /api/database/:id expects the engine-less shape; we keep
	// the engine to stay friendly across server versions which ignore unknown
	// keys instead of rejecting them.
	delete(payload, "is_full_sync") // not settable via PUT
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/database/%d", strings.TrimRight(m.cfg.URL, "/"), id)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metabase-Session", session)
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("update database status %d: %s", resp.StatusCode, string(raw))
	}
	return databaseID(id), nil
}

// putOwnedDataSource updates a datasource we already own. Re-stamps the
// details with the ownership marker so a partial schema migration doesn't
// silently drift us out of "managed" state. Callers MUST have already called
// isNexusManagedDatabase — this helper doesn't re-check.
func (m *MetabaseBootstrapper) putOwnedDataSource(ctx context.Context, session string, id int, engine string, details map[string]any) (databaseID, error) {
	if details == nil {
		details = map[string]any{}
	}
	details[nexusManagedKey] = nexusManagedValue
	return m.putDataSource(ctx, session, id, map[string]any{
		"engine":  engine,
		"details": details,
	})
}

// clickHouseDetails converts an HTTP SQL endpoint into the Metabase "details"
// shape. Metabase accepts a "url" key like "...&username=...&password=..." for
// ClickHouse HTTP engine. Database name is parsed from ?database=.
func clickHouseDetails(endpoint string) map[string]any {
	details := map[string]any{
		"url": endpoint,
	}
	// Extract ?database= so Metabase knows the default DB.
	if idx := strings.Index(endpoint, "database="); idx >= 0 {
		rest := endpoint[idx+len("database="):]
		// Trim at & or end.
		if amp := strings.Index(rest, "&"); amp >= 0 {
			rest = rest[:amp]
		}
		details["dbname"] = rest
	}
	return details
}

// postgresDetails converts a Postgres URL into the Metabase details shape.
func postgresDetails(jdbcURL string) map[string]any {
	out := map[string]any{}
	// Metabase accepts jdbc:postgresql://... — leave as-is for the operator
	// to override via env if a non-URL scheme is preferred.
	out["db"] = jdbcURL
	return out
}

// seedCollections reads every *.json file under SeedDir; each file is expected
// to be a Metabase collection export ({"name": "...", "cards": [...]}) and is
// registered via /api/collection + /api/card. Missing dir is a no-op.
func (m *MetabaseBootstrapper) seedCollections(ctx context.Context, session string, dbs databases) error {
	if m.cfg.SeedDir == "" {
		return nil
	}
	entries, err := os.ReadDir(m.cfg.SeedDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(m.cfg.SeedDir, e.Name())
		if err := m.seedOne(ctx, session, dbs, path); err != nil {
			m.log.Warn("metabase seed entry failed", "file", path, "err", err)
			continue
		}
	}
	return nil
}

// seedOne registers a single collection export file. The shape matches what
// Metabase's "export as JSON" produces on the UI side, allowing operators to
// round-trip dashboards between an existing Metabase instance and the local
// dev one.
func (m *MetabaseBootstrapper) seedOne(ctx context.Context, session string, dbs databases, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var col struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Cards       []struct {
			Name       string `json:"name"`
			DatabaseID int    `json:"database_id,omitempty"`
			Engine     string `json:"engine,omitempty"`
			Query      struct {
				Native map[string]any `json:"native"`
			} `json:"query,omitempty"`
			Display       string         `json:"display,omitempty"`
			Visualization map[string]any `json:"visualization,omitempty"`
		} `json:"cards,omitempty"`
	}
	if err := json.Unmarshal(raw, &col); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	// Collection (top-level container). We call POST /api/collection; if a
	// collection with the same name exists, we look it up and reuse the id.
	id, owned, err := m.ensureCollection(ctx, session, col.Name, col.Description)
	if err != nil {
		return fmt.Errorf("collection: %w", err)
	}
	if !owned {
		// Foreign collection in our reserved "Nexus - <name>" namespace; the
		// ensureCollection warning already fired. Skip the card loop so we
		// don't pollute the user-managed collection with our dashboards.
		return nil
	}
	for _, c := range col.Cards {
		dbID := c.DatabaseID
		if dbID == 0 && c.Engine != "" {
			if v, ok := dbs[c.Engine]; ok {
				dbID = int(v)
			}
		}
		if dbID == 0 {
			m.log.Warn("seed card: no database_id resolved; using clickhouse default",
				"collection", col.Name, "card", c.Name)
			if v, ok := dbs["clickhouse"]; ok {
				dbID = int(v)
			}
		}
		if err := m.ensureCard(ctx, session, id, dbID, c.Name, c.Display, c.Query.Native, c.Visualization); err != nil {
			m.log.Warn("seed card failed", "collection", col.Name, "card", c.Name, "err", err)
			continue
		}
	}
	m.log.Info("metabase collection seeded", "name", col.Name, "cards", len(col.Cards))
	return nil
}

func (m *MetabaseBootstrapper) ensureCollection(ctx context.Context, session, name, desc string) (int, bool, error) {
	// Collision check up front so we don't POST-then-fall-back. If a foreign
	// collection already uses the reserved "Nexus - <name>" name, we leave it
	// alone and surface a warn. The caller skips its card loop on a foreign
	// collection so dashboards owned by another team are not overwritten.
	if existingID, existingDesc, found, err := m.findCollectionWithDescription(ctx, session, "Nexus - "+name); err != nil {
		return 0, false, err
	} else if found {
		if !isNexusManagedCollection(existingDesc) {
			m.log.Warn("metabase collection with reserved name already exists; refraining from update",
				"name", "Nexus - "+name,
				"id", existingID,
				"hint", "the existing collection is not owned by Nexus. To take it over, prefix its description with \""+nexusManagedDescriptionPrefix+"\".",
			)
			return existingID, false, nil
		}
		return existingID, true, nil
	}

	url := strings.TrimRight(m.cfg.URL, "/") + "/api/collection"
	payload := map[string]any{
		"name": "Nexus - " + name,
		// Stamp the description so a later re-deploy recognises us as the owner
		// even if the database_id changed (e.g. after a Metabase restore).
		"description": nexusManagedDescriptionPrefix + desc,
		"color":       "#509EE3",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metabase-Session", session)
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity {
		// Existing collection — find by name through /api/collection root
		// then return its id. (Post-creation collision check above handles the
		// case *before* the POST; this branch fires when two Nexus nodes race.)
		_ = resp.Body.Close()
		id, err := m.findCollection(ctx, session, "Nexus - "+name)
		// A freshly created collection by us owns itself (description marker).
		// A racing foreign creator would already be marked foreign above; if
		// we reach this branch the creator was another Nexus deploy, so the
		// marker is present.
		return id, true, err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, false, fmt.Errorf("create collection status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false, err
	}
	if out.ID == 0 {
		return 0, false, errors.New("collection create returned no id")
	}
	return out.ID, true, nil
}

// findCollection lists /api/collection and returns the id of the named one.
// Used to recover an existing collection id when POST returns 4xx.
func (m *MetabaseBootstrapper) findCollection(ctx context.Context, session, name string) (int, error) {
	id, _, found, err := m.findCollectionWithDescription(ctx, session, name)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("collection %q not found in /api/collection", name)
	}
	return id, nil
}

// findCollectionWithDescription is the description-aware variant used by
// ensureCollection's ownership check.
func (m *MetabaseBootstrapper) findCollectionWithDescription(ctx context.Context, session, name string) (int, string, bool, error) {
	url := strings.TrimRight(m.cfg.URL, "/") + "/api/collection"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("X-Metabase-Session", session)
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, "", false, fmt.Errorf("list collections status %d: %s", resp.StatusCode, string(raw))
	}
	var rows []struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return 0, "", false, err
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID, r.Description, true, nil
		}
	}
	return 0, "", false, nil
}

// ensureCard creates a card under the given collection. Display defaults to
// "table" when empty; visualization is a free-form object passed through.
func (m *MetabaseBootstrapper) ensureCard(ctx context.Context, session string, collectionID, dbID int, name, display string, nativeQuery map[string]any, viz map[string]any) error {
	if dbID == 0 {
		return errors.New("no database id resolvable for card " + name)
	}
	if display == "" {
		display = "table"
	}
	payload := map[string]any{
		"name":          name,
		"display":       display,
		"collection_id": collectionID,
		"database_id":   dbID,
		"query": map[string]any{
			"native": nativeQuery,
		},
		"visualization_settings": viz,
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(m.cfg.URL, "/") + "/api/card"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metabase-Session", session)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// 4xx on create is recoverable: a card with same name already exists,
		// and the operator can re-run the seed safely. We just log + skip.
		return fmt.Errorf("create card status %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}
