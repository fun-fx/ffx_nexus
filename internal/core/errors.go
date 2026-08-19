package core

import "errors"

// ErrUnauthenticated is the placeholder error returned by handlers
// when the request lacks authentication. The resp.HTTP wrapper pairs
// it with apierr.CodeUnauthorized so the customer never sees
// internal Go error text on the wire.
var ErrUnauthenticated = errors.New("unauthenticated")
