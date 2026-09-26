// Package storedoc is the drift-proof bridge between domain structs and
// cumulite's map-currency documents.
//
// It exists because of one incident: a store hand-maintained a
// struct→map translation table, a new struct field missed the table, and
// the loss reached storage silently — reads decode the whole document, so
// only the missing field's readers could ever notice. The fix is to never
// hand-maintain the table at all:
//
//   - Doc derives the document from the struct's json tags (one marshal),
//     which is what the capability path produces too;
//   - Write prefers cumulite's StructPort (the engine's typed path) and
//     falls back to Doc + Insert/Replace only for ports that lack the
//     capability (test doubles, hypothetical remote adapters).
//
// Both paths carry every tagged field, so a new field can no longer be
// dropped by a translator that forgot it.
package storedoc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/willove/cumulite"
)

// Doc derives a document from any value via its json tags. The result has
// the same keys the engine's typed write path produces, so a store can
// use either without changing what lands in storage.
func Doc(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("storedoc: marshal %T: %w", v, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("storedoc: document shape of %T: %w", v, err)
	}
	return m, nil
}

// WriteStruct stores doc through the port: StructPort when the engine
// offers it, Doc + Insert/Replace otherwise. insertFn/replaceFn close
// over the collection and keep the caller's insert-or-replace decision.
func WriteStruct(ctx context.Context, c cumulite.Port, coll, id string, v any, exists bool) error {
	if sp, ok := c.(cumulite.StructPort); ok {
		if exists {
			_, err := sp.ReplaceStruct(ctx, coll, id, v)
			return err
		}
		_, err := sp.InsertStructs(ctx, coll, []any{v})
		return err
	}
	doc, derr := Doc(v)
	if derr != nil {
		return derr
	}
	if exists {
		_, ierr := c.ReplaceDocument(ctx, coll, id, doc)
		return ierr
	}
	_, ierr := c.Insert(ctx, coll, []map[string]any{doc})
	return ierr
}

// DeclareShape registers a collection's canonical shape (the zero value's
// json tags) so any write that reaches storage through another path — a
// patch, a future map — is audited against the struct. Best effort: a
// port without ShapePort simply keeps the un-audited behaviour.
func DeclareShape(ctx context.Context, c cumulite.Port, coll string, shape any) {
	if sp, ok := c.(cumulite.ShapePort); ok {
		_ = sp.SetCollectionShape(ctx, coll, shape)
	}
}
