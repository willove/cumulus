// Package modelprofile is the model-endpoint registry: multiple saved
// endpoint profiles (base URL / key / chat+embed model), one of them ACTIVE.
//
// The engine itself keeps reading plain env (LLM_*) — every request rebuilds
// the stack from env, which is what makes activation cheap: activating a
// profile MATERIALIZES it (writes .env + sets process env) and the next request
// picks it up with zero stack changes. The registry is the durable, enumerable
// view of what an operator has configured; .env stays the single materialized
// truth so CLI and serve share one mechanism.
//
// API keys are stored in the local store only and never echoed back (the HTTP
// face follows /v1/config's api_key_set convention). Same trust model as .env
// itself: single operator, loopback serve.
package modelprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/ns"
)

const (
	RegistryPrefix = "clus:model:"
	IndexKey       = "clus:models"
	ActiveKey      = "clus:model:active"
)

// Profile is one saved endpoint configuration. APIKey is write-only through
// every reader (marshaled for storage, stripped at the HTTP face).
type Profile struct {
	ID             string    `json:"id"`
	Label          string    `json:"label,omitempty"`
	BaseURL        string    `json:"base_url"`
	ChatModel      string    `json:"chat_model,omitempty"`
	EmbedModel     string    `json:"embed_model,omitempty"`
	APIKey         string    `json:"api_key,omitempty"`
	ReasoningSplit bool      `json:"reasoning_split,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// LastUsedAt is stamped on activation — "which profile is actually
	// serving" is the operator's first question when consumption looks wrong.
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// Store is the profile registry over a cumulite Port (bucket.Store pattern:
// one KV record per entry, a single index document for enumeration).
type Store struct {
	c cumulite.Port
}

func New(c cumulite.Port) *Store { return &Store{c: c} }

// Save creates or updates a profile. ID must validate as an ns segment
// (letters/digits/_-., no spaces) so it composes into the KV key safely.
// An update with an empty APIKey keeps the stored key (the UI never echoes
// it back, so "leave blank = unchanged" is the only workable round-trip).
func (s *Store) Save(ctx context.Context, p Profile) (Profile, error) {
	if err := ns.Validate(p.ID); err != nil {
		return Profile{}, fmt.Errorf("model profile: %w", err)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return Profile{}, fmt.Errorf("model profile: base_url required")
	}
	now := time.Now().UTC()
	existing, err := s.Get(ctx, p.ID)
	if err != nil {
		return Profile{}, err
	}
	if existing != nil {
		p.CreatedAt = existing.CreatedAt
		if strings.TrimSpace(p.APIKey) == "" {
			p.APIKey = existing.APIKey
		}
	} else {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if err := s.put(ctx, p); err != nil {
		return Profile{}, err
	}
	if existing == nil {
		if err := s.addToIndex(ctx, p.ID); err != nil {
			return Profile{}, err
		}
	}
	return p, nil
}

// Get returns one profile or nil when absent.
func (s *Store) Get(ctx context.Context, id string) (*Profile, error) {
	if err := ns.Validate(id); err != nil {
		return nil, err
	}
	raw, err := s.c.KVGet(ctx, RegistryPrefix+id)
	if err != nil {
		if contract.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("model profile %s: %w", id, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("model profile %s: %w", id, err)
	}
	return &p, nil
}

// List returns every profile, label-sorted for a stable UI.
func (s *Store) List(ctx context.Context) ([]Profile, error) {
	raw, err := s.c.KVGet(ctx, IndexKey)
	if err != nil || len(raw) == 0 {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("model profile index: %w", err)
	}
	out := make([]Profile, 0, len(ids))
	for _, id := range ids {
		p, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if p == nil {
			continue // index is a hint; the record is the truth
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// Remove drops a profile. Removing the ACTIVE profile is refused — deactivate
// by activating another one first (an active-but-deleted profile would leave
// .env pointing at a ghost with no way to inspect it).
func (s *Store) Remove(ctx context.Context, id string) error {
	if err := ns.Validate(id); err != nil {
		return err
	}
	active, err := s.ActiveID(ctx)
	if err != nil {
		return err
	}
	if active == id {
		return fmt.Errorf("model profile %q is active — activate another profile first", id)
	}
	if _, err := s.c.KVDelete(ctx, RegistryPrefix+id); err != nil {
		return err
	}
	return s.removeFromIndex(ctx, id)
}

// SetActive records the activation pointer and stamps LastUsedAt.
func (s *Store) SetActive(ctx context.Context, id string) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("model profile %q not found", id)
	}
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	if err := s.c.KVPut(ctx, ActiveKey, raw, 0); err != nil {
		return err
	}
	p.LastUsedAt = time.Now().UTC()
	return s.put(ctx, *p)
}

// ActiveID returns the active profile id, or "" when nothing was activated
// through the registry (e.g. env-configured deployments pre-dating profiles).
func (s *Store) ActiveID(ctx context.Context) (string, error) {
	raw, err := s.c.KVGet(ctx, ActiveKey)
	if err != nil || len(raw) == 0 {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", fmt.Errorf("model profile active pointer: %w", err)
	}
	return id, nil
}

func (s *Store) put(ctx context.Context, p Profile) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, RegistryPrefix+p.ID, raw, 0)
}

func (s *Store) addToIndex(ctx context.Context, id string) error {
	ids, err := s.index(ctx)
	if err != nil {
		return err
	}
	for _, v := range ids {
		if v == id {
			return nil
		}
	}
	ids = append(ids, id)
	return s.writeIndex(ctx, ids)
}

func (s *Store) removeFromIndex(ctx context.Context, id string) error {
	ids, err := s.index(ctx)
	if err != nil {
		return err
	}
	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return s.writeIndex(ctx, out)
}

func (s *Store) index(ctx context.Context) ([]string, error) {
	raw, err := s.c.KVGet(ctx, IndexKey)
	if err != nil || len(raw) == 0 {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *Store) writeIndex(ctx context.Context, ids []string) error {
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, IndexKey, raw, 0)
}
