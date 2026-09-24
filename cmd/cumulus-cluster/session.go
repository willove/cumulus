package main

// Session persistence: chat sessions live in the store's KV under
// clus:session:<id> — one JSON document per session holding the message
// stream. `cumulus-cluster search -session <id>` folds the recent turns into the history
// rewriter and appends the new turn afterwards; the HTTP /v1/search body
// accepts the same "session" field. The web workbench's ChatThreads reads the
// same keys. With -ns / "ns" the keys are scoped as ns:<name>:clus:session:<id>.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/ns"
)

// sessionWriteMu serializes session turns. A turn is a read-modify-write of one
// KV document, so two concurrent turns on the same session would both read the
// old message list and the later save would drop the other turn. A turn costs
// one KV put, so a single lock for all sessions costs nothing measurable and
// avoids a per-session lock map that would grow without bound.
var sessionWriteMu sync.Mutex

const sessionKeyPrefix = "clus:session:"

// sessionMessage is one turn in the stream.
type sessionMessage struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
	At      int64  `json:"at"`
}

// sessionDoc is the KV payload.
type sessionDoc struct {
	ID        string           `json:"id"`
	Title     string           `json:"title,omitempty"`
	CreatedAt int64            `json:"created_at"`
	UpdatedAt int64            `json:"updated_at"`
	Messages  []sessionMessage `json:"messages"`
}

// historyLines folds the recent turns into history-rewrite input lines.
func (s *sessionDoc) historyLines(max int) []string {
	out := make([]string, 0, min(len(s.Messages), max))
	for _, m := range s.Messages {
		out = append(out, m.Role+": "+m.Content)
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

func newSessionID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type sessionStore struct {
	c  cumulite.Port
	ns string // namespace scope — "" = default library KV keys
}

func (st sessionStore) key(id string) string { return ns.KV(st.ns, sessionKeyPrefix+id) }

func (st sessionStore) load(ctx context.Context, id string) (*sessionDoc, error) {
	raw, err := st.c.KVGet(ctx, st.key(id))
	if err != nil {
		return nil, err
	}
	var d sessionDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	return &d, nil
}

func (st sessionStore) save(ctx context.Context, d *sessionDoc) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return st.c.KVPut(ctx, st.key(d.ID), raw, 0)
}

// ensure loads the session or, if absent, creates one titled by the first query.
func (st sessionStore) ensure(ctx context.Context, id, title string) (*sessionDoc, error) {
	if d, err := st.load(ctx, id); err == nil {
		return d, nil
	} else if !contract.IsNotFound(err) {
		return nil, err
	}
	now := time.Now().UnixMilli()
	return &sessionDoc{ID: id, Title: title, CreatedAt: now, UpdatedAt: now}, nil
}

func (st sessionStore) appendTurn(ctx context.Context, id, title, query, answer string) (*sessionDoc, error) {
	sessionWriteMu.Lock()
	defer sessionWriteMu.Unlock()
	d, err := st.ensure(ctx, id, title)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if d.Title == "" {
		d.Title = title
	}
	d.Messages = append(d.Messages,
		sessionMessage{Role: "user", Content: query, At: now},
		sessionMessage{Role: "assistant", Content: answer, At: now},
	)
	d.UpdatedAt = now
	return d, st.save(ctx, d)
}

// create is ensure-then-save under the same lock appendTurn takes: the pair is
// one read-modify-write, so the HTTP face must not run it unguarded either.
func (st sessionStore) create(ctx context.Context, id, title string) (*sessionDoc, error) {
	sessionWriteMu.Lock()
	defer sessionWriteMu.Unlock()
	d, err := st.ensure(ctx, id, title)
	if err != nil {
		return nil, err
	}
	return d, st.save(ctx, d)
}

func (st sessionStore) list(ctx context.Context) ([]*sessionDoc, error) {
	keys, err := st.c.KVKeys(ctx, ns.KV(st.ns, sessionKeyPrefix), 1000)
	if err != nil {
		return nil, err
	}
	var out []*sessionDoc
	for _, k := range keys {
		raw, err := st.c.KVGet(ctx, k)
		if err != nil {
			continue
		}
		var d sessionDoc
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		out = append(out, &d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// sessionHistory loads the session's recent turns as rewrite-history lines;
// a missing session yields an empty history (first turn).
func sessionHistory(ctx context.Context, st sessionStore, id string, max int) ([]string, *sessionDoc, error) {
	d, err := st.load(ctx, id)
	if contract.IsNotFound(err) {
		return nil, &sessionDoc{ID: id}, nil // first turn: create on append
	}
	if err != nil {
		return nil, nil, err
	}
	return d.historyLines(max), d, nil
}

// --- CLI face ---------------------------------------------------------------

func runSessionCLI(ctx context.Context, c cumulite.Port, args []string, namespace string) {
	st := sessionStore{c: c, ns: namespace}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "new":
		id := newSessionID()
		d := &sessionDoc{ID: id, Title: "会话 " + id, CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli()}
		if err := st.save(ctx, d); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"id": id})
	case "list":
		all, err := st.list(ctx)
		if err != nil {
			fatal(err)
		}
		printJSON(all)
	case "show":
		if len(args) < 1 {
			fatal(fmt.Errorf("session show: id required"))
		}
		d, err := st.load(ctx, args[0])
		if err != nil {
			fatal(err)
		}
		printJSON(d)
	case "rm":
		if len(args) < 1 {
			fatal(fmt.Errorf("session rm: id required"))
		}
		ok, err := st.c.KVDelete(ctx, st.key(args[0]))
		if err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"deleted": ok})
	default:
		fatal(fmt.Errorf("session: unknown subcommand %q (new|list|show|rm)", sub))
	}
}
