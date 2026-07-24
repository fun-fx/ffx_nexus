package gateway

// Scope describes the visibility class of a registered provider/router:
//
//   - Public:  shipped by the operator (or a tenant) and visible to every
//     account in the org. This is what /v1/models advertises to anyone who
//     holds a virtual key, and what every authenticated console user sees.
//   - Org:     a tenant-scoped router created by the org admin. Visible to
//     every member of the same org_id, but not to other orgs.
//   - User:    a per-account router created by the user from a BYOK
//     credential. Visible only to the owning user (and admins).
//
// The legacy registry treated all providers as Public. The ConsoleCatalog
// surfaces this triplet to the Playground picker so the UI can show
// "Team" vs "Personal" badges; the API filter (see PR #2) enforces the
// visibility rules server-side so a member of one org cannot enumerate
// another org's user-routers.
type Scope string

const (
	ScopePublic Scope = "public"
	ScopeOrg    Scope = "org"
	ScopeUser   Scope = "user"
)

// ScopeHint is the metadata the registry attaches to a registration so
// lookups can later report the visibility class of each model. OwnerID is
// only meaningful when Scope == ScopeUser; for org/public routers it stays
// empty.
type ScopeHint struct {
	Scope   Scope
	OwnerID string
}

// IsZero reports whether the hint carries no information (the zero value).
// Callers use it as a fallback to ScopePublic so old registrations made
// before the hint existed still appear as the historical default.
func (h ScopeHint) IsZero() bool { return h.Scope == "" }
