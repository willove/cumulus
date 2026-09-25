package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

const QueueLimit = 8
const MaxRecords = 200
const MaxAttempts = 3

var ErrNotFound = errors.New("evaluation object not found")
var ErrConflict = errors.New("evaluation conflict")

// Record is the durable experiment, including the complete immutable corpus.
// Results and progress are committed together in one atomic KV value: progress
// can never point past a result which has not been persisted.
type Record struct {
	Run        Run             `json:"run"`
	Items      []Item          `json:"items"`
	Corpus     []source.Source `json:"corpus"`
	Results    []ItemResult    `json:"results"`
	RequestID  string          `json:"request_id"`
	RequestSHA string          `json:"request_sha"`
	Owner      string          `json:"owner"`
}
type Executor interface {
	Execute(context.Context, Item, int64) ItemResult
	Close() error
}
type Factory func(context.Context, Record) (Executor, error)
type Work struct{ Namespace, ID string }
type Service struct {
	c           cumulite.Port
	factory     Factory
	fingerprint func(Config) string
	mu          sync.Mutex
	owner       string
	ctx         context.Context
	stop        context.CancelFunc
	finished    chan struct{}
	queue       chan Work
	cancels     map[Work]context.CancelFunc
	// A bounded warm-state cache. Eviction makes retries explicitly unavailable;
	// we never rebuild a cold experiment and call it the same retry.
	warm      map[Work]Executor
	reserved  map[Work]Executor // queued retries cannot be evicted from the cache
	warmOrder []Work
}

func NewService(ctx context.Context, c cumulite.Port, f Factory, fingerprint func(Config) string) *Service {
	ctx, stop := context.WithCancel(ctx)
	s := &Service{c: c, factory: f, fingerprint: fingerprint, owner: NewID(), ctx: ctx, stop: stop, finished: make(chan struct{}), queue: make(chan Work, QueueLimit), cancels: map[Work]context.CancelFunc{}, warm: map[Work]Executor{}, reserved: map[Work]Executor{}}
	go s.worker()
	return s
}
func (s *Service) Close() { s.stop(); <-s.finished }
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func key(namespace, kind, id string) string { return ns.KV(namespace, "clus:eval-v2:"+kind+":"+id) }
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func (s *Service) read(ctx context.Context, k string, v any) error {
	b, err := s.c.KVGet(ctx, k)
	if contract.IsNotFound(err) || err == nil && len(b) == 0 {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func (s *Service) put(ctx context.Context, k string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, k, b, 0)
}
func (s *Service) keys(ctx context.Context, namespace, kind string) ([]string, error) {
	return s.c.KVKeys(ctx, key(namespace, kind, ""), MaxRecords+1)
}
func (s *Service) SaveDataset(ctx context.Context, namespace, name, content string, corpus []source.Source) (Dataset, Validation, error) {
	v := ValidateDataset(content, corpus)
	if !v.Valid {
		return Dataset{}, v, fmt.Errorf("dataset validation failed")
	}
	if strings.TrimSpace(name) == "" || len(name) > 200 {
		return Dataset{}, v, fmt.Errorf("name required (maximum 200 bytes)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.keys(ctx, namespace, "dataset")
	if err != nil {
		return Dataset{}, v, err
	}
	if len(keys) >= MaxRecords {
		return Dataset{}, v, fmt.Errorf("dataset storage limit reached (%d)", MaxRecords)
	}
	d := Dataset{ID: NewID(), Name: name, SHA: v.SHA, Count: len(v.Items), CreatedAt: time.Now().UTC(), Items: v.Items}
	return d, v, s.put(ctx, key(namespace, "dataset", d.ID), d)
}
func (s *Service) Dataset(ctx context.Context, namespace, id string) (Dataset, error) {
	var d Dataset
	if !validID(id) {
		return d, ErrNotFound
	}
	err := s.read(ctx, key(namespace, "dataset", id), &d)
	return d, err
}
func (s *Service) Datasets(ctx context.Context, namespace string) ([]Dataset, error) {
	keys, err := s.keys(ctx, namespace, "dataset")
	if err != nil {
		return nil, err
	}
	out := []Dataset{}
	for _, k := range keys {
		var d Dataset
		if err := s.read(ctx, k, &d); err != nil {
			return nil, err
		}
		d.Items = nil
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Fingerprints cover the selected items in execution order, all source fields,
// and the effective model/runtime configuration rather than just UI switches.
func Binding(items []Item, corpus []source.Source, configText string) Frozen {
	a, _ := json.Marshal(items)
	b, _ := json.Marshal(corpus)
	return Freeze(a, b, []byte(configText), 0)
}
func (s *Service) Start(ctx context.Context, namespace, datasetID, name, requestID string, cfg Config, corpus []source.Source) (Run, error) {
	if err := cfg.Validate(true, true); err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return Run{}, fmt.Errorf("evaluation service stopped")
	}
	if requestID == "" || len(requestID) > 100 || len(name) > 200 {
		return Run{}, fmt.Errorf("request_id required (maximum 100 bytes); name maximum 200 bytes")
	}
	requestBytes, _ := json.Marshal([]any{datasetID, name, cfg})
	requestSHA := HashBytes(requestBytes)
	keys, err := s.keys(ctx, namespace, "run")
	if err != nil {
		return Run{}, err
	}
	for _, k := range keys {
		var rec Record
		if err := s.read(ctx, k, &rec); err != nil {
			return Run{}, err
		}
		if rec.RequestID == requestID {
			if rec.RequestSHA != requestSHA {
				return Run{}, fmt.Errorf("%w: request_id already used with different parameters", ErrConflict)
			}
			if err := s.recover(ctx, namespace, &rec); err != nil {
				return Run{}, err
			}
			return rec.Run, nil
		}
	}
	if len(keys) >= MaxRecords {
		return Run{}, fmt.Errorf("run storage limit reached (%d)", MaxRecords)
	}
	if len(s.queue) >= cap(s.queue) {
		return Run{}, fmt.Errorf("%w: evaluation queue full; try later", ErrConflict)
	}
	d, err := s.Dataset(ctx, namespace, datasetID)
	if err != nil {
		return Run{}, err
	}
	// Recheck gold resolution against the actual run snapshot, not save-time state.
	for _, it := range d.Items {
		for _, g := range it.Gold {
			found := false
			for _, src := range corpus {
				if src.Status == source.StatusActive && (g == src.ID || g == src.BusinessKey) {
					found = true
					break
				}
			}
			if !found {
				return Run{}, fmt.Errorf("gold source %q is no longer active; save a corrected dataset", g)
			}
		}
	}
	sort.Slice(corpus, func(i, j int) bool { return corpus[i].ID < corpus[j].ID })
	raw, err := json.Marshal(corpus)
	if err != nil {
		return Run{}, err
	}
	if len(raw) > MaxSnapshotBytes {
		return Run{}, fmt.Errorf("corpus snapshot exceeds %d bytes", MaxSnapshotBytes)
	}
	// JSON roundtrip detaches all nested source maps from caller-owned memory.
	var frozenCorpus []source.Source
	if err := json.Unmarshal(raw, &frozenCorpus); err != nil {
		return Run{}, err
	}
	items := d.Items
	if cfg.Limit > 0 && cfg.Limit < len(items) {
		items = items[:cfg.Limit]
	}
	now := time.Now().UTC()
	text := s.fingerprint(cfg)
	run := Run{ID: NewID(), Name: name, DatasetID: d.ID, DatasetName: d.Name, Protocol: Protocol, State: "queued", Total: len(items), CreatedAt: now, UpdatedAt: now, Config: cfg, ConfigText: text, Frozen: Binding(items, frozenCorpus, text)}
	rec := Record{Run: run, Items: items, Corpus: frozenCorpus, Results: []ItemResult{}, RequestID: requestID, RequestSHA: requestSHA, Owner: s.owner}
	if err := s.put(ctx, key(namespace, "run", run.ID), rec); err != nil {
		return Run{}, err
	}
	s.queue <- Work{namespace, run.ID}
	return run, nil
}
func (s *Service) recover(ctx context.Context, namespace string, rec *Record) error {
	if rec.Owner != s.owner && Active(rec.Run.State) {
		rec.Run.State = "interrupted"
		rec.Run.Error = "server restarted; isolated warm state was lost; start a new run"
		rec.Run.CurrentItem = ""
		rec.Run.UpdatedAt = time.Now().UTC()
		return s.put(ctx, key(namespace, "run", rec.Run.ID), rec)
	}
	return nil
}
func (s *Service) load(ctx context.Context, w Work) (Record, error) {
	var rec Record
	if !validID(w.ID) {
		return rec, ErrNotFound
	}
	if err := s.read(ctx, key(w.Namespace, "run", w.ID), &rec); err != nil {
		return rec, err
	}
	err := s.recover(ctx, w.Namespace, &rec)
	return rec, err
}
func (s *Service) Get(ctx context.Context, namespace, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(ctx, Work{namespace, id})
}
func (s *Service) Runs(ctx context.Context, namespace string) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.keys(ctx, namespace, "run")
	if err != nil {
		return nil, err
	}
	out := []Run{}
	for _, k := range keys {
		var rec Record
		if err := s.read(ctx, k, &rec); err != nil {
			return nil, err
		}
		if err := s.recover(ctx, namespace, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec.Run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
func (s *Service) Cancel(ctx context.Context, namespace, id string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := Work{namespace, id}
	rec, err := s.load(ctx, w)
	if err != nil {
		return Run{}, err
	}
	if !Active(rec.Run.State) {
		return rec.Run, nil
	}
	rec.Run.State = "cancelling"
	if cancel := s.cancels[w]; cancel != nil {
		cancel()
	} else {
		rec.Run.State = "cancelled"
	}
	rec.Run.UpdatedAt = time.Now().UTC()
	err = s.put(ctx, key(namespace, "run", id), rec)
	return rec.Run, err
}
func (s *Service) Retry(ctx context.Context, namespace, id string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := Work{namespace, id}
	rec, err := s.load(ctx, w)
	if err != nil {
		return Run{}, err
	}
	if Active(rec.Run.State) {
		return Run{}, fmt.Errorf("%w: run is still active", ErrConflict)
	}
	if rec.Run.Protocol != Protocol || rec.Run.ConfigText != s.fingerprint(rec.Run.Config) || rec.Run.Frozen != Binding(rec.Items, rec.Corpus, rec.Run.ConfigText) {
		return Run{}, fmt.Errorf("%w: protocol or frozen configuration changed; start a new run", ErrConflict)
	}
	if s.warm[w] == nil {
		return Run{}, fmt.Errorf("%w: isolated experiment state not preserved (restart or cache eviction); start a new run", ErrConflict)
	}
	if rec.Run.Failed == 0 {
		return Run{}, fmt.Errorf("%w: no failed items to retry", ErrConflict)
	}
	for _, r := range rec.Results {
		if r.State == "failed" && r.Attempt >= MaxAttempts {
			return Run{}, fmt.Errorf("%w: maximum %d attempts reached; start a new run", ErrConflict, MaxAttempts)
		}
	}
	if rec.Run.Summary.Tokens() >= rec.Run.Config.TokenBudget {
		return Run{}, fmt.Errorf("%w: token budget exhausted; start a new run", ErrConflict)
	}
	if len(s.queue) >= cap(s.queue) {
		return Run{}, fmt.Errorf("%w: queue full", ErrConflict)
	}
	rec.Run.State = "queued"
	rec.Run.Error = ""
	rec.Run.UpdatedAt = time.Now().UTC()
	if err := s.put(ctx, key(namespace, "run", id), rec); err != nil {
		return Run{}, err
	}
	s.reserved[w] = s.warm[w]
	delete(s.warm, w)
	for i, k := range s.warmOrder {
		if k == w {
			s.warmOrder = append(s.warmOrder[:i], s.warmOrder[i+1:]...)
			break
		}
	}
	s.queue <- w
	return rec.Run, nil
}
func (s *Service) save(rec *Record, w Work) error {
	rec.Run.UpdatedAt = time.Now().UTC()
	rec.Run.Summary = Summarize(rec.Results)
	rec.Run.Done = len(rec.Results)
	rec.Run.Failed = 0
	for _, r := range rec.Results {
		if r.State == "failed" {
			rec.Run.Failed++
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.put(ctx, key(w.Namespace, "run", w.ID), rec)
}
func (s *Service) worker() {
	defer close(s.finished)
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, e := range s.warm {
			_ = e.Close()
		}
		for _, e := range s.reserved {
			_ = e.Close()
		}
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case w := <-s.queue:
			s.execute(w)
		}
	}
}
func (s *Service) execute(w Work) {
	s.mu.Lock()
	rec, err := s.load(s.ctx, w)
	if err != nil || rec.Run.State != "queued" || s.ctx.Err() != nil {
		if e := s.reserved[w]; e != nil {
			_ = e.Close()
			delete(s.reserved, w)
		}
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(rec.Run.Config.Timeout)*time.Second)
	s.cancels[w] = cancel
	executor := s.reserved[w]
	retry := executor != nil
	delete(s.reserved, w)
	rec.Run.State = "running"
	err = s.save(&rec, w)
	s.mu.Unlock()
	defer cancel()
	if err == nil && executor == nil {
		executor, err = s.factory(ctx, rec)
	}
	if err == nil {
		for _, it := range rec.Items {
			previous := -1
			for i, r := range rec.Results {
				if r.ID == it.ID {
					previous = i
					break
				}
			}
			if retry && (previous < 0 || rec.Results[previous].State != "failed") {
				continue
			}
			if ctx.Err() != nil {
				err = ctx.Err()
				break
			}
			remaining := rec.Run.Config.TokenBudget - rec.Run.Summary.Tokens()
			if remaining <= 0 {
				err = fmt.Errorf("total token budget exhausted; in-flight stage costs may exceed the limit")
				break
			}
			s.mu.Lock()
			rec.Run.CurrentItem = it.ID
			err = s.save(&rec, w)
			s.mu.Unlock()
			if err != nil {
				break
			}
			ictx, icancel := context.WithTimeout(ctx, time.Duration(rec.Run.Config.ItemTimeout)*time.Second)
			result := executor.Execute(ictx, it, remaining)
			if ictx.Err() != nil && result.Error == "" {
				result.State = "failed"
				result.Error = ictx.Err().Error()
				result.ErrorStage = "timeout"
			}
			icancel()
			result.ID = it.ID
			result.Query = it.Query
			result.Reference = it.Answer
			result.Gold = it.Gold
			result.Attempt = 1
			if result.Citations == nil {
				result.Citations = []Citation{}
			}
			if previous >= 0 {
				old := rec.Results[previous]
				result.Attempt = old.Attempt + 1
				history := old.Attempts
				old.Attempts = nil
				result.Attempts = append(history, old)
				rec.Results[previous] = result
			} else {
				rec.Results = append(rec.Results, result)
			}
			s.mu.Lock()
			err = s.save(&rec, w)
			s.mu.Unlock()
			if err != nil {
				break
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, w)
	rec.Run.CurrentItem = ""
	rec.Run.State = "completed"
	switch {
	case s.ctx.Err() != nil:
		rec.Run.State = "interrupted"
		rec.Run.Error = "server stopped; start a new run"
	case ctx.Err() == context.Canceled:
		rec.Run.State = "cancelled"
		rec.Run.Error = "cancelled by user"
	case err != nil:
		rec.Run.State = "failed"
		rec.Run.Error = err.Error()
	case rec.Run.Failed > 0:
		rec.Run.State = "failed"
		rec.Run.Error = "one or more items failed; inspect item errors"
	case rec.Run.Done != rec.Run.Total:
		rec.Run.State = "failed"
		rec.Run.Error = "run has unstarted items; retry only reruns failed items; start a new run for the full dataset"
	}
	// Never retain state after an unsuccessful persistence write. A retry cannot
	// claim the saved history describes an uncommitted mutation of the experiment.
	saveErr := s.save(&rec, w)
	if executor != nil {
		if rec.Run.Failed > 0 && saveErr == nil && s.ctx.Err() == nil {
			if len(s.warmOrder) >= 4 {
				oldest := s.warmOrder[0]
				s.warmOrder = s.warmOrder[1:]
				_ = s.warm[oldest].Close()
				delete(s.warm, oldest)
			}
			s.warm[w] = executor
			s.warmOrder = append(s.warmOrder, w)
		} else {
			_ = executor.Close()
		}
	}
}
