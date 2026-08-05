package benchmark

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// resolveEnvironmentIDs maps Hub slugs such as "primeintellect/gsm8k"
// to the internal ids the hosted-evaluations create endpoint expects.
//
// The vendor documents environment_ids as slugs, but create rejects
// them with a 404; GET /environmentshub/{owner}/{name}/status is the
// supported lookup path. Values that already look like internal ids
// (no slash) are passed through unchanged.
func (c *Client) resolveEnvironmentIDs(ctx context.Context, slugs []string) ([]string, error) {
	out := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		id, err := c.resolveEnvironmentID(ctx, slug)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func (c *Client) resolveEnvironmentID(ctx context.Context, slug string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("%w: environment slug must not be empty", ErrInvalidRequest)
	}
	// Internal ids are opaque strings with no slash; slugs are owner/name.
	if !strings.Contains(slug, "/") {
		return slug, nil
	}
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("%w: environment slug %q must be owner/name", ErrInvalidRequest, slug)
	}
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/api/v1/environmentshub/%s/%s/status", owner, name)
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", err
	}
	if resp.Data.ID == "" {
		return "", fmt.Errorf("benchmark: environment %q resolved to an empty id", slug)
	}
	return resp.Data.ID, nil
}
