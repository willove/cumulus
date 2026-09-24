package main

// Flag ordering for subcommands that take BOTH flags and positional arguments.
//
// Go's flag package stops at the first non-flag argument, so
// `ingest-adapt file.jsonl -job b` silently turns "-job" and "b" into two more
// positional files (and the -job flag keeps its default). The same trap bit
// `bucket new law -label X` earlier. This reorders the argument list so every
// flag token — and the value that follows a non-boolean flag — precedes the
// positionals, which is what the flag package needs.
//
// boolFlags lists flags that take no value; anything else consumes the next
// token. Unknown flags are treated as boolean (they will be reported by Parse).

import "strings"

// flagsFirst moves flag tokens ahead of positionals, preserving relative order
// inside each group. "--" terminates flag parsing, as in the flag package.
func flagsFirst(args []string, boolFlags ...string) []string {
	isBool := map[string]bool{}
	for _, f := range boolFlags {
		isBool[f] = true
	}
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Keep the terminator itself: flag.Parse stops there, which is the
			// documented escape hatch for a positional that looks like a flag.
			rest = append(rest, args[i:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue // -flag=value already carries its own value
		}
		if !isBool[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, rest...)
}
