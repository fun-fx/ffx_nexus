package observability

import "testing"

func TestNormalizeMetabaseID(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want int
		err  bool
	}{
		{"nil", nil, 0, false},
		{"int", int(42), 42, false},
		{"int64", int64(99), 99, false},
		{"float64-int", float64(7), 7, false},
		{"string-int", "55", 55, false},
		{"string-int-quoted", "\"55\"", 55, false},
		{"string-empty", "", 0, false},
		{"string-empty-quoted", "\"\"", 0, false},
		{"string-hex", "2a", 42, false},
		{"string-junk", "xyz", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeMetabaseID(tc.raw)
			if (err != nil) != tc.err {
				t.Fatalf("error: got %v, want %v", err, tc.err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFindCollectionRowsStringIDs mimics Metabase 0.49+/0.50 returning
// `id` as a string, which the previous int-typed decoder rejected.
func TestFindCollectionRowsStringIDs(t *testing.T) {
	// The unpack step in findCollectionWithDescription does this for
	// every row of /api/collection. Verify normalizeMetabaseID lets us
	// accept a string-encoded id round-trip into int.
	row := struct {
		ID          any
		Name        string
		Description string
	}{ID: "47", Name: "Nexus - 01 - Overview"}
	id, err := normalizeMetabaseID(row.ID)
	if err != nil || id != 47 {
		t.Fatalf("got id=%d err=%v, want 47/nil", id, err)
	}
}
