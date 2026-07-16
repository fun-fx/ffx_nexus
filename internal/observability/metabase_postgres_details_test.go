package observability

import (
	"reflect"
	"testing"
)

// TestPostgresDetailsMirrorsClickhouseShape asserts that the verbose
// shape pattern used for ClickHouse carries over for Postgres:
// {host, port, dbname, user, password}. Metabase's postgres-jdbc
// driver accepts the verbose form and rejects a URL-only `db`
// field on registration with:
//
//	"Unable to parse URL jdbc:postgresql://localhost:5432/<user-url>"
//
// The verbose form was already adopted for clickHouseDetails; this
// test pins it for postgres so a future refactor that falls back
// to the URL shortcut is caught in CI.
func TestPostgresDetailsMirrorsClickhouseShape(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantKeys []string
		noKeys   []string // keys that must NOT be present in the verbose form
	}{
		{
			name:     "verbose url with port and dbname",
			in:       "postgresql://nexus:pw@postgres-nexus-rw.tenant-nexus.svc.cluster.local:5432/nexus",
			wantKeys: []string{"host", "port", "dbname", "user", "password"},
			noKeys:   []string{"db", "connection-string"},
		},
		{
			name:     "sslmode=disable maps to ssl=false",
			in:       "postgresql://nexus:pw@host:5432/nexus?sslmode=disable",
			wantKeys: []string{"host", "port", "dbname", "user", "password", "ssl"},
			noKeys:   []string{"db"},
		},
		{
			name:     "sslmode=require maps to ssl=true",
			in:       "postgresql://nexus:pw@host:5432/nexus?sslmode=require",
			wantKeys: []string{"ssl"},
		},
		{
			name:     "user without password omits password field",
			in:       "postgresql://nexus@host:5432/nexus",
			wantKeys: []string{"host", "user", "dbname"},
			noKeys:   []string{"password"},
		},
		{
			name:     "default port 5432",
			in:       "postgresql://nexus:pw@host/nexus",
			wantKeys: []string{"host", "port", "dbname", "user", "password"},
		},
		{
			name:     "no url falls back to connection-string",
			in:       "host:5432:foo:bar:baz",
			wantKeys: []string{"connection-string"},
		},
		{
			name:     "empty input returns empty",
			in:       "",
			wantKeys: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := postgresDetails(tc.in)
			for _, k := range tc.wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("missing key %q in result: %#v", k, got)
				}
			}
			for _, k := range tc.noKeys {
				if _, ok := got[k]; ok {
					t.Errorf("unexpected key %q in result (should not be present): %#v", k, got)
				}
			}
		})
	}
}

// TestPostgresDetailsHostAndPortParity asserts the host/port extraction
// works exactly like clickHouseDetails — same library semantics, the
// operator-visible difference between port=9000 and port=5432
// shouldn't surprise a future refactor that unifies the two helpers.
func TestPostgresDetailsHostAndPortParity(t *testing.T) {
	d := postgresDetails("postgresql://u:p@db.example.com:6000/widgets")
	if d["host"] != "db.example.com" {
		t.Errorf("host = %v, want db.example.com", d["host"])
	}
	if port, _ := d["port"].(int); port != 6000 {
		t.Errorf("port = %v, want 6000", d["port"])
	}
	if d["dbname"] != "widgets" {
		t.Errorf("dbname = %v, want widgets", d["dbname"])
	}
	if d["user"] != "u" {
		t.Errorf("user = %v, want u", d["user"])
	}
	if d["password"] != "p" {
		t.Errorf("password = %v, want p", d["password"])
	}
	keys := map[string]bool{}
	for k := range d {
		keys[k] = true
	}
	for _, want := range []string{"host", "port", "dbname", "user", "password"} {
		if !keys[want] {
			t.Errorf("missing %q in keys %v", want, reflect.ValueOf(d).MapKeys())
		}
	}
}
