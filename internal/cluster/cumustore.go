package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/cumudb/pkg/client"
)

// CumuStore persists clusters in a cumudb collection (default ask_clusters).
type CumuStore struct {
	c    *client.Client
	coll string
}

func NewCumuStore(c *client.Client, coll string) *CumuStore {
	if coll == "" {
		coll = "ask_clusters"
	}
	return &CumuStore{c: c, coll: coll}
}

func (s *CumuStore) Save(ctx context.Context, c Cluster) error {
	doc := map[string]any{
		"_id":        c.ID,
		"topic_key":  c.TopicKey,
		"name":       c.Name,
		"content":    c.Content,
		"queries":    c.Queries,
		"embed":      c.Embed,
		"confidence": c.Confidence,
		"hotness":    c.Hotness,
		"lifecycle":  c.Lifecycle,
		"version":    c.Version,
		"source_id":  c.SourceID,
		"evidence":   evidenceToAny(c.Evidence),
		"flags":      c.Flags,
		"created_at": c.CreatedAt.Format(time.RFC3339Nano),
		"updated_at": c.UpdatedAt.Format(time.RFC3339Nano),
	}
	if len(c.TopicKeys) > 0 {
		doc["topic_keys"] = c.TopicKeys
	}
	if len(c.LevelKeys) > 0 {
		doc["level_keys"] = c.LevelKeys
	}
	if len(c.KeyEmbeds) > 0 {
		doc["key_embeds"] = c.KeyEmbeds
	}
	// Insert-or-replace by _id (content-stable id).
	if existing, err := s.c.GetDocument(ctx, s.coll, c.ID); err == nil && existing != nil {
		_, err := s.c.ReplaceDocument(ctx, s.coll, c.ID, doc)
		return err
	}
	_, err := s.c.Insert(ctx, s.coll, []map[string]any{doc})
	return err
}

func (s *CumuStore) Get(ctx context.Context, id string) (*Cluster, error) {
	d, err := s.c.GetDocument(ctx, s.coll, id)
	if err != nil {
		if client.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return fromDoc(d)
}

func (s *CumuStore) FindByTopic(ctx context.Context, topicKey string) ([]Cluster, error) {
	res, err := s.c.Query(ctx, s.coll, client.Query{
		Filter: map[string]any{"$or": []map[string]any{
			{"topic_key": topicKey},
			{"topic_keys": map[string]any{"$in": []string{topicKey}}},
		}},
		Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(res.Documents))
	for _, d := range res.Documents {
		c, err := fromDoc(d)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *CumuStore) All(ctx context.Context) ([]Cluster, error) {
	res, err := s.c.Query(ctx, s.coll, client.Query{Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(res.Documents))
	for _, d := range res.Documents {
		c, err := fromDoc(d)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *CumuStore) Delete(ctx context.Context, id string) error {
	ok, err := s.c.DeleteDocument(ctx, s.coll, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cluster %s not found", id)
	}
	return nil
}

func evidenceToAny(ev []mcs.Sample) []any {
	out := make([]any, 0, len(ev))
	for _, s := range ev {
		doc := map[string]any{
			"start": s.Start, "end": s.End, "content": s.Content,
			"source": s.Source, "score": s.Score, "reasoning": s.Reasoning,
		}
		// B5/B6 annotations ride along when present (omitempty shapes).
		if s.Arm != "" {
			doc["arm"] = s.Arm
		}
		if len(s.Covers) > 0 {
			doc["covers"] = s.Covers
		}
		out = append(out, doc)
	}
	return out
}

func fromDoc(d map[string]any) (*Cluster, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var c Cluster
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decoding cluster: %w", err)
	}
	if c.ID == "" {
		if id, ok := d["_id"].(string); ok {
			c.ID = id
		}
	}
	return &c, nil
}
