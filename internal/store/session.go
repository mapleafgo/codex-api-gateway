// Package store keeps recent Responses context for previous_response_id enrichment.
package store

import (
	"container/list"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

const sessionKeyPrefix = "session:"

// Entry stores one response's effective context plus which source produced it.
type Entry struct {
	SourceName string
	Context    []contextItem
	Items      []model.OutputItem
	size       int64
}

type contextItem struct {
	Type             string             `json:"type"`
	Role             string             `json:"role,omitempty"`
	Text             string             `json:"text,omitempty"`
	ID               string             `json:"id,omitempty"`
	CallID           string             `json:"call_id,omitempty"`
	Name             string             `json:"name,omitempty"`
	Arguments        string             `json:"arguments,omitempty"`
	Output           string             `json:"output,omitempty"`
	Summary          []model.OutputText `json:"summary,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
}

type storedEntry struct {
	key  string
	size int64
	elem *list.Element
}

// SessionStore holds recent response contexts keyed by response_id.
type SessionStore struct {
	mu            sync.Mutex
	db            *badger.DB
	entries       map[string]*storedEntry
	lru           *list.List
	bytes         int64
	maxBytes      int64
	maxEntryBytes int64
	ttl           time.Duration
}

// New creates a SessionStore. maxBytes/maxEntryBytes<=0 disables that byte limit;
// ttl<=0 disables expiry for tests.
func New(maxBytes, maxEntryBytes int64, ttl time.Duration) *SessionStore {
	opts := badger.DefaultOptions("").WithInMemory(true).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		panic(err)
	}
	return newStore(db, maxBytes, maxEntryBytes, ttl)
}

// Open creates a SessionStore backed by a Badger database at path.
func Open(path string, maxBytes, maxEntryBytes int64, ttl time.Duration) (*SessionStore, error) {
	if path == "" {
		return nil, errors.New("session store path is required")
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}
	opts := badger.DefaultOptions(path).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	s := newStore(db, maxBytes, maxEntryBytes, ttl)
	if err := s.loadIndex(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.mu.Lock()
	s.enforceMaxBytesLocked()
	s.mu.Unlock()
	return s, nil
}

func newStore(db *badger.DB, maxBytes, maxEntryBytes int64, ttl time.Duration) *SessionStore {
	return &SessionStore{
		db:            db,
		entries:       map[string]*storedEntry{},
		lru:           list.New(),
		maxBytes:      maxBytes,
		maxEntryBytes: maxEntryBytes,
		ttl:           ttl,
	}
}

// Close closes the underlying Badger database.
func (s *SessionStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Save stores output items for a response id.
func (s *SessionStore) Save(responseID, sourceName string, items []model.OutputItem) {
	s.SaveContext(responseID, sourceName, nil, items)
}

// SaveResponse stores the effective request input plus generated output for a response id.
func (s *SessionStore) SaveResponse(responseID, sourceName string, req *oairesponses.ResponseNewParams, items []model.OutputItem) {
	var input []oairesponses.ResponseInputItemUnionParam
	if req != nil {
		input = inputContext(req)
	}
	s.SaveContext(responseID, sourceName, input, items)
}

// SaveContext stores the effective conversation context for a response id.
func (s *SessionStore) SaveContext(responseID, sourceName string, input []oairesponses.ResponseInputItemUnionParam, items []model.OutputItem) {
	if responseID == "" {
		return
	}
	entry := Entry{
		SourceName: sourceName,
		Context:    buildContext(input, items),
		Items:      cloneOutputItems(items),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		slog.Error("序列化会话上下文失败", "response_id", responseID, "error", err)
		return
	}
	size := int64(len(responseID) + len(data))
	if (s.maxEntryBytes > 0 && size > s.maxEntryBytes) || (s.maxBytes > 0 && size > s.maxBytes) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.deleteLocked(responseID)
		return
	}
	if err := s.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry(sessionKey(responseID), data)
		if s.ttl > 0 {
			e = e.WithTTL(s.ttl)
		}
		return txn.SetEntry(e)
	}); err != nil {
		slog.Error("保存会话上下文失败", "response_id", responseID, "error", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.putIndexLocked(responseID, size)
	s.enforceMaxBytesLocked()
}

// Get returns a stored entry if present.
func (s *SessionStore) Get(responseID string) (Entry, bool) {
	entry, ok := s.loadEntry(responseID)
	if !ok {
		s.mu.Lock()
		s.removeIndexLocked(responseID)
		s.mu.Unlock()
		return Entry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putIndexLocked(responseID, entry.size)
	return cloneEntry(entry), true
}

// Delete removes the entry for responseID, if any.
func (s *SessionStore) Delete(responseID string) {
	if responseID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(responseID)
}

// Enrich prepends stored context to req.Input when previous_response_id is set.
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
	prefix := contextForSource(e.Context, sameSource)
	if len(prefix) == 0 && len(e.Items) > 0 {
		prefix = outputItemsForSource(e.Items, sameSource)
	}
	current := inputContext(req)
	req.Input.OfString = oparam.Opt[string]{}
	req.Input.OfInputItemList = append(prefix, current...)
	return outputItemsForSignature(e.Items, sameSource)
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

func (s *SessionStore) enforceMaxBytesLocked() {
	for s.maxBytes > 0 && s.bytes > s.maxBytes {
		elem := s.lru.Back()
		if elem == nil {
			return
		}
		key, _ := elem.Value.(string)
		s.deleteLocked(key)
	}
}

func (s *SessionStore) deleteLocked(responseID string) {
	if err := s.db.Update(func(txn *badger.Txn) error {
		err := txn.Delete(sessionKey(responseID))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		return err
	}); err != nil {
		slog.Warn("删除会话上下文失败", "response_id", responseID, "error", err)
	}
	s.removeIndexLocked(responseID)
}

func (s *SessionStore) putIndexLocked(responseID string, size int64) {
	if existing, ok := s.entries[responseID]; ok {
		s.bytes -= existing.size
		existing.size = size
		s.bytes += size
		s.lru.MoveToFront(existing.elem)
		return
	}
	elem := s.lru.PushFront(responseID)
	s.entries[responseID] = &storedEntry{key: responseID, size: size, elem: elem}
	s.bytes += size
}

func (s *SessionStore) removeIndexLocked(responseID string) {
	entry, ok := s.entries[responseID]
	if !ok {
		return
	}
	s.bytes -= entry.size
	s.lru.Remove(entry.elem)
	delete(s.entries, responseID)
}

func (s *SessionStore) loadIndex() error {
	prefix := []byte(sessionKeyPrefix)
	return s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = true
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			data, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			var entry Entry
			if err := json.Unmarshal(data, &entry); err != nil {
				return err
			}
			responseID := string(item.KeyCopy(nil)[len(sessionKeyPrefix):])
			s.putIndexLocked(responseID, int64(len(responseID)+len(data)))
		}
		return nil
	})
}

func (s *SessionStore) loadEntry(responseID string) (Entry, bool) {
	var entry Entry
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(sessionKey(responseID))
		if err != nil {
			return err
		}
		data, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		entry.size = int64(len(responseID) + len(data))
		return nil
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return Entry{}, false
	}
	if err != nil {
		slog.Warn("读取会话上下文失败", "response_id", responseID, "error", err)
		return Entry{}, false
	}
	return entry, true
}

func sessionKey(responseID string) []byte {
	return []byte(sessionKeyPrefix + responseID)
}

func cloneEntry(entry Entry) Entry {
	entry.Context = cloneContextItems(entry.Context)
	entry.Items = cloneOutputItems(entry.Items)
	return entry
}

func cloneOutputItems(items []model.OutputItem) []model.OutputItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]model.OutputItem, len(items))
	copy(out, items)
	for i := range out {
		out[i].Content = cloneOutputText(out[i].Content)
		out[i].Summary = cloneOutputText(out[i].Summary)
	}
	return out
}

func cloneOutputText(items []model.OutputText) []model.OutputText {
	if len(items) == 0 {
		return nil
	}
	out := make([]model.OutputText, len(items))
	copy(out, items)
	return out
}

func inputContext(req *oairesponses.ResponseNewParams) []oairesponses.ResponseInputItemUnionParam {
	var out []oairesponses.ResponseInputItemUnionParam
	if req.Input.OfString.Valid() && req.Input.OfString.Value != "" {
		out = append(out, oairesponses.ResponseInputItemUnionParam{
			OfMessage: &oairesponses.EasyInputMessageParam{
				Role: oairesponses.EasyInputMessageRoleUser,
				Content: oairesponses.EasyInputMessageContentUnionParam{
					OfString: oparam.NewOpt(req.Input.OfString.Value),
				},
			},
		})
	}
	out = append(out, req.Input.OfInputItemList...)
	return cloneInputItems(out)
}

func buildContext(input []oairesponses.ResponseInputItemUnionParam, items []model.OutputItem) []contextItem {
	out := make([]contextItem, 0, len(input)+len(items))
	for _, item := range input {
		if converted, ok := inputItemToContext(item); ok {
			out = append(out, converted)
		}
	}
	for _, item := range items {
		out = append(out, outputItemToContext(item))
	}
	return out
}

func contextForSource(items []contextItem, sameSource bool) []oairesponses.ResponseInputItemUnionParam {
	out := make([]oairesponses.ResponseInputItemUnionParam, 0, len(items))
	for _, item := range items {
		if item.Type == model.ItemTypeReasoning && !sameSource {
			continue
		}
		out = append(out, contextToInputItem(item))
	}
	return out
}

func outputItemsForSource(items []model.OutputItem, sameSource bool) []oairesponses.ResponseInputItemUnionParam {
	out := make([]oairesponses.ResponseInputItemUnionParam, 0, len(items))
	for _, it := range items {
		if it.Type == model.ItemTypeReasoning && !sameSource {
			continue
		}
		out = append(out, toInputItemParam(it))
	}
	return out
}

func outputItemsForSignature(items []model.OutputItem, sameSource bool) []model.OutputItem {
	if sameSource {
		return cloneOutputItems(items)
	}
	out := make([]model.OutputItem, 0, len(items))
	for _, item := range items {
		if item.Type == model.ItemTypeReasoning {
			continue
		}
		out = append(out, item)
	}
	return cloneOutputItems(out)
}

func cloneInputItems(items []oairesponses.ResponseInputItemUnionParam) []oairesponses.ResponseInputItemUnionParam {
	if len(items) == 0 {
		return nil
	}
	data, err := json.Marshal(items)
	if err != nil {
		out := make([]oairesponses.ResponseInputItemUnionParam, len(items))
		copy(out, items)
		return out
	}
	var out []oairesponses.ResponseInputItemUnionParam
	if err := json.Unmarshal(data, &out); err != nil {
		out = make([]oairesponses.ResponseInputItemUnionParam, len(items))
		copy(out, items)
	}
	return out
}

func inputItemToContext(item oairesponses.ResponseInputItemUnionParam) (contextItem, bool) {
	if item.OfMessage != nil {
		return contextItem{
			Type: model.ItemTypeMessage,
			Role: string(item.OfMessage.Role),
			Text: messageParamText(item.OfMessage),
		}, true
	}
	if item.OfReasoning != nil {
		ctx := contextItem{
			Type: model.ItemTypeReasoning,
			ID:   item.OfReasoning.ID,
		}
		for _, summary := range item.OfReasoning.Summary {
			ctx.Summary = append(ctx.Summary, model.OutputText{
				Type: model.ContentTypeSummaryText,
				Text: summary.Text,
			})
		}
		if item.OfReasoning.EncryptedContent.Valid() {
			ctx.EncryptedContent = item.OfReasoning.EncryptedContent.Value
		}
		return ctx, true
	}
	if item.OfFunctionCall != nil {
		return contextItem{
			Type:      model.ItemTypeFunctionCall,
			CallID:    item.OfFunctionCall.CallID,
			Name:      item.OfFunctionCall.Name,
			Arguments: item.OfFunctionCall.Arguments,
		}, true
	}
	if item.OfFunctionCallOutput != nil {
		return contextItem{
			Type:   model.ItemTypeFunctionCallOutput,
			CallID: item.OfFunctionCallOutput.CallID,
			Output: item.OfFunctionCallOutput.Output.OfString.Value,
		}, true
	}
	return contextItem{}, false
}

func outputItemToContext(item model.OutputItem) contextItem {
	switch item.Type {
	case model.ItemTypeMessage:
		return contextItem{
			Type: model.ItemTypeMessage,
			Role: item.Role,
			Text: outputText(item.Content),
		}
	case model.ItemTypeFunctionCall:
		return contextItem{
			Type:      model.ItemTypeFunctionCall,
			ID:        item.ID,
			CallID:    item.CallID,
			Name:      item.Name,
			Arguments: item.Arguments,
		}
	case model.ItemTypeReasoning:
		return contextItem{
			Type:             model.ItemTypeReasoning,
			ID:               item.ID,
			Summary:          cloneOutputText(item.Summary),
			EncryptedContent: item.EncryptedContent,
		}
	default:
		return contextItem{Type: item.Type}
	}
}

func contextToInputItem(item contextItem) oairesponses.ResponseInputItemUnionParam {
	switch item.Type {
	case model.ItemTypeMessage:
		role := oairesponses.EasyInputMessageRole(item.Role)
		if role == "" {
			role = oairesponses.EasyInputMessageRoleAssistant
		}
		return oairesponses.ResponseInputItemUnionParam{
			OfMessage: &oairesponses.EasyInputMessageParam{
				Role: role,
				Content: oairesponses.EasyInputMessageContentUnionParam{
					OfString: oparam.NewOpt(item.Text),
				},
			},
		}
	case model.ItemTypeFunctionCall:
		return oairesponses.ResponseInputItemUnionParam{
			OfFunctionCall: &oairesponses.ResponseFunctionToolCallParam{
				CallID:    item.CallID,
				Name:      item.Name,
				Arguments: item.Arguments,
			},
		}
	case model.ItemTypeFunctionCallOutput:
		return oairesponses.ResponseInputItemUnionParam{
			OfFunctionCallOutput: &oairesponses.ResponseInputItemFunctionCallOutputParam{
				CallID: item.CallID,
				Output: oairesponses.ResponseInputItemFunctionCallOutputOutputUnionParam{
					OfString: oparam.NewOpt(item.Output),
				},
			},
		}
	case model.ItemTypeReasoning:
		r := &oairesponses.ResponseReasoningItemParam{ID: item.ID}
		for _, summary := range item.Summary {
			r.Summary = append(r.Summary, oairesponses.ResponseReasoningItemSummaryParam{
				Text: summary.Text,
			})
		}
		if item.EncryptedContent != "" {
			r.EncryptedContent = oparam.NewOpt(item.EncryptedContent)
		}
		return oairesponses.ResponseInputItemUnionParam{OfReasoning: r}
	default:
		return oairesponses.ResponseInputItemUnionParam{}
	}
}

func messageParamText(message *oairesponses.EasyInputMessageParam) string {
	var parts []string
	if message.Content.OfString.Valid() && message.Content.OfString.Value != "" {
		parts = append(parts, message.Content.OfString.Value)
	}
	for _, content := range message.Content.OfInputItemContentList {
		if content.OfInputText != nil && content.OfInputText.Text != "" {
			parts = append(parts, content.OfInputText.Text)
		}
	}
	return joinText(parts)
}

func outputText(items []model.OutputText) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	return joinText(parts)
}

func joinText(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out += "\n" + part
	}
	return out
}

func cloneContextItems(items []contextItem) []contextItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]contextItem, len(items))
	copy(out, items)
	for i := range out {
		out[i].Summary = cloneOutputText(out[i].Summary)
	}
	return out
}
