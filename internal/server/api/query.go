package api

import (
	"errors"

	"github.com/rakunlabs/ada"
	"github.com/rakunlabs/query"

	"github.com/rakunlabs/kutu/internal/service"
)

// mustValidator builds a query.Validator that whitelists the given field
// names for both filtering (?field=...) and sorting (_sort=field). Panics
// at startup on a malformed validator (programmer error).
func mustValidator(fields ...string) *query.Validator {
	v, err := query.NewValidator(
		query.WithValues(query.WithIn(fields...)),
		query.WithSort(query.WithIn(fields...)),
	)
	if err != nil {
		panic("api: build query validator: " + err.Error())
	}
	return v
}

// parseListQuery parses the request URL query string into a *query.Query
// and validates it against the whitelist. Unknown filter/sort fields are
// rejected with 400 so only allowed columns ever reach SQL.
func parseListQuery(c *ada.Context, v *query.Validator) (*query.Query, error) {
	q, err := query.Parse(c.Request.URL.RawQuery)
	if err != nil {
		return nil, errors.Join(err, service.ErrBadRequest)
	}
	if v != nil {
		if err := q.Validate(v); err != nil {
			return nil, errors.Join(err, service.ErrBadRequest)
		}
	}
	return q, nil
}
