package main

// Bucket CLI face: the registry that names the corpus partitions and the gate
// that makes a retrieval name one. Buckets ARE namespaces (same composite
// identity, same isolation); what this adds over raw -ns is enumeration and a
// refusal when nobody named one.

import (
	"context"
	"fmt"
	"os"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/bucket"
)

func runBucketCLI(ctx context.Context, c cumulite.Port, rest []string) {
	store := bucket.New(c)
	sub := "list"
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "list":
		all, err := store.List(ctx)
		if err != nil {
			fatal(err)
		}
		printJSON(all)
	case "new", "create":
		// Positional, not flags: the flag package stops parsing at the first
		// positional, so `bucket new law -label X` would silently drop the
		// label. Same reasoning as the codebase's other subcommands.
		if len(rest) < 2 {
			fatal(fmt.Errorf("bucket new: name required (usage: bucket new <name> [label] [note])"))
		}
		label, note := "", ""
		if len(rest) >= 3 {
			label = rest[2]
		}
		if len(rest) >= 4 {
			note = rest[3]
		}
		b, err := store.Create(ctx, rest[1], label, note)
		if err != nil {
			fatal(err)
		}
		printJSON(b)
	case "rm", "delete":
		if len(rest) < 2 {
			fatal(fmt.Errorf("bucket rm: name required"))
		}
		if err := store.Remove(ctx, rest[1]); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"removed": rest[1]})
	case "show":
		if len(rest) < 2 {
			fatal(fmt.Errorf("bucket show: name required"))
		}
		b, err := store.Get(ctx, rest[1])
		if err != nil {
			fatal(err)
		}
		if b == nil {
			fatal(fmt.Errorf("bucket %q is not registered", rest[1]))
		}
		printJSON(b)
	case "require":
		// Diagnostics for the gate itself: prints what a query face would say.
		if len(rest) < 2 {
			fatal(fmt.Errorf("bucket require: name required"))
		}
		if err := store.Require(ctx, rest[1]); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"ok": rest[1]})
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
