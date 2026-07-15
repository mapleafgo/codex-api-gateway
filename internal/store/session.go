// Package store keeps recent Responses output for previous_response_id enrichment.
package store

import (
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// DefaultMaxEntries is used when callers pass maxEntries=0.
const DefaultMaxEntries = 10000

// Entry stores one response's output plus which source produced it.
type Entry struct {
	SourceName string
	Items      []model.OutputItem
	expiresAt  time.Time
}

// SessionStore holds recent response outputs keyed by response_id.
type SessionStore struct {
	mu      sync.Mutex
	entries map[string]Entry
	max     int
	ttl     time.Duration
}

// New creates a SessionStore. maxEntries<0 means unlimited; ttl<=0 disables expiry for tests.
func New(maxEntries int, ttl time.Duration) *SessionStore {
	if maxEntries == 0 {
		maxEntries = DefaultMaxEntries
	}
	return &SessionStore{entries: map[string]Entry{}, max: maxEntries, ttl: ttl}
}

// Save stores output items for a response id.
func (s *SessionStore) Save(responseID, sourceName string, items []model.OutputItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := time.Time{}
	if s.ttl > 0 {
		exp = time.Now().Add(s.ttl)
	}
	s.entries[responseID] = Entry{SourceName: sourceName, Items: items, expiresAt: exp}
	if s.max > 0 && len(s.entries) > s.max {
		s.evictLocked()
	}
}

// Get returns a stored entry if present.
func (s *SessionStore) Get(responseID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[responseID]
	if !ok {
		return Entry{}, false
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(s.entries, responseID)
		return Entry{}, false
	}
	return e, true
}

// Enrich prepends stored output items to req.Input when previous_response_id is set.
// Reasoning items are dropped when targetSource differs from the producing source
// (cross-source signature invalid). Function call items are always kept.
// Returns the stored items that were enriched (for signature lookup in convert).
func (s *SessionStore) Enrich(req *oairesponses.ResponseNewParams, targetSource string) []model.OutputItem {
	if !req.PreviousResponseID.Valid() || req.PreviousResponseID.Value == "" {
		return nil
	}
	e, ok := s.Get(req.PreviousResponseID.Value)
	if !ok {
		return nil
	}
	sameSource := e.SourceName == targetSource
	prefix := make([]oairesponses.ResponseInputItemUnionParam, 0, len(e.Items))
	for _, it := range e.Items {
		if it.Type == model.ItemTypeReasoning && !sameSource {
			continue
		}
		prefix = append(prefix, toInputItemParam(it))
	}
	req.Input.OfInputItemList = append(prefix, req.Input.OfInputItemList...)
	return e.Items
}

// toInputItemParam converts a stored OutputItem back to an SDK input item.
func toInputItemParam(it model.OutputItem) oairesponses.ResponseInputItemUnionParam {
	switch it.Type {
	case model.ItemTypeMessage:
		role := oairesponses.EasyInputMessageRole(it.Role)
		if role == "" {
			role = oairesponses.EasyInputMessageRoleAssistant
		}
		// Extract text from content parts.
		var text string
		for _, c := range it.Content {
			if c.Text != "" {
				if text != "" {
					text += "\n"
				}
				text += c.Text
			}
		}
		return oairesponses.ResponseInputItemUnionParam{
			OfMessage: &oairesponses.EasyInputMessageParam{
				Role: role,
				Content: oairesponses.EasyInputMessageContentUnionParam{
					OfString: oparam.NewOpt(text),
				},
			},
		}
	case model.ItemTypeFunctionCall:
		return oairesponses.ResponseInputItemUnionParam{
			OfFunctionCall: &oairesponses.ResponseFunctionToolCallParam{
				CallID:    it.CallID,
				Name:      it.Name,
				Arguments: it.Arguments,
			},
		}
	case model.ItemTypeReasoning:
		r := &oairesponses.ResponseReasoningItemParam{
			ID: it.ID,
		}
		for _, s := range it.Summary {
			r.Summary = append(r.Summary, oairesponses.ResponseReasoningItemSummaryParam{
				Text: s.Text,
			})
		}
		if it.EncryptedContent != "" {
			r.EncryptedContent = oparam.NewOpt(it.EncryptedContent)
		}
		return oairesponses.ResponseInputItemUnionParam{
			OfReasoning: r,
		}
	}
	return oairesponses.ResponseInputItemUnionParam{}
}

func (s *SessionStore) evictLocked() {
	now := time.Now()
	for k, e := range s.entries {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			delete(s.entries, k)
			return
		}
	}
	for k := range s.entries {
		delete(s.entries, k)
		return
	}
}
