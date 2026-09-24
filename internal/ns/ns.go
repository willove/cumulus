// Package ns scopes the ask suite to one store namespace. The engine's
// composite collection identity ("ns:coll", NS 线) is the isolation unit:
// a bare name is the default library and a composite identity is a separate
// collection, never an alias. Every collection, KV job cursor and session key
// the suite owns is composed through this package so a tenant's reuse path
// (clusters, weak edges, sessions) can never see another tenant's.
package ns

import (
	"errors"
	"fmt"
)

// ErrInvalidNamespace reports a namespace segment the engine would reject.
var ErrInvalidNamespace = errors.New("invalid namespace")

// Coll composes one collection identity: "" (default library) returns the
// bare base name, a namespace returns "<ns>:<base>". Callers pass full
// identities everywhere else — the engine treats bare and composite as
// distinct collections and never migrates one onto the other.
func Coll(namespace, base string) string {
	if namespace == "" {
		return base
	}
	return namespace + ":" + base
}

// KV scopes a flat KV key to a namespace. The engine's KV keyspace is opaque,
// so the suite keeps one prefix convention: "ns:<namespace>:<key>" (default
// library keys stay unprefixed).
func KV(namespace, key string) string {
	if namespace == "" {
		return key
	}
	return "ns:" + namespace + ":" + key
}

// Validate keeps a namespace segment composable into an identity: 1..128
// bytes, letters, digits, '_', '.' and '-', no leading '_' (the reserve that
// keeps the default library's bare keys unambiguous) and no leading '.' or
// '-'. A ':' inside would make the identity triple-composite and ambiguous.
func Validate(namespace string) error {
	if namespace == "" {
		return nil // default library: nothing to validate
	}
	if len(namespace) > 128 {
		return fmt.Errorf("%w: length must be 1..128", ErrInvalidNamespace)
	}
	for i := 0; i < len(namespace); i++ {
		c := namespace[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
			if i == 0 {
				return fmt.Errorf("%w: %q must not start with %q", ErrInvalidNamespace, namespace, string(c))
			}
		default:
			return fmt.Errorf("%w: %q contains illegal byte %q", ErrInvalidNamespace, namespace, string(c))
		}
	}
	return nil
}
