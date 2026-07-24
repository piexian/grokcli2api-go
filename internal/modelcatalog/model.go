// Package modelcatalog contains the credential-independent model catalog
// contract shared by request renderers and the credential pool.
package modelcatalog

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// Backend is the upstream wire protocol selected for a model.
type Backend string

const (
	BackendChatCompletions Backend = "chat_completions"
	BackendResponses       Backend = "responses"
	BackendMessages        Backend = "messages"
)

// ModelDescriptor is an immutable snapshot of the routing and capability
// metadata advertised for one account/model pair. Remote transport and auth
// override fields (baseUrl/apiKey/envKey) are intentionally not represented:
// they must never replace operator-controlled origins or credentials.
type ModelDescriptor struct {
	ID                       string        `json:"id"`
	WireModel                string        `json:"model"`
	Backend                  Backend       `json:"api_backend"`
	ContextWindow            uint64        `json:"context_window,omitempty"`
	MaxCompletionTokens      uint32        `json:"max_completion_tokens,omitempty"`
	SupportsReasoningEffort  bool          `json:"supports_reasoning_effort"`
	ReasoningEfforts         []string      `json:"reasoning_efforts,omitempty"`
	SupportsBackendSearch    bool          `json:"supports_backend_search"`
	StreamToolCalls          bool          `json:"stream_tool_calls"`
	Hidden                   bool          `json:"hidden"`
	SupportedInAPI           bool          `json:"supported_in_api"`
	InferenceIdleTimeout     time.Duration `json:"-"`
	InferenceIdleTimeoutSecs uint64        `json:"inference_idle_timeout_secs,omitempty"`
	MaxRetries               *uint32       `json:"max_retries,omitempty"`
	AutoCompactThreshold     *uint8        `json:"auto_compact_threshold_percent,omitempty"`
	CompactionAtTokens       *uint64       `json:"compaction_at_tokens,omitempty"`
	CompactionsRemaining     *uint8        `json:"compactions_remaining,omitempty"`
	Created                  int64         `json:"created,omitempty"`
}

// Normalize returns a safe descriptor. Unknown backends use the historical
// Chat Completions default and invalid/duplicate reasoning entries disappear.
func (d ModelDescriptor) Normalize() ModelDescriptor {
	d.ID = strings.TrimSpace(d.ID)
	d.WireModel = strings.TrimSpace(d.WireModel)
	if d.ID == "" {
		d.ID = d.WireModel
	}
	if d.WireModel == "" {
		d.WireModel = d.ID
	}
	d.Backend = Backend(strings.ToLower(strings.TrimSpace(string(d.Backend))))
	switch d.Backend {
	case BackendChatCompletions, BackendResponses, BackendMessages:
	default:
		d.Backend = BackendChatCompletions
	}
	seen := make(map[string]struct{}, len(d.ReasoningEfforts))
	reasoning := make([]string, 0, len(d.ReasoningEfforts))
	for _, effort := range d.ReasoningEfforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if !isCanonicalReasoningEffort(effort) {
			continue
		}
		if _, exists := seen[effort]; exists {
			continue
		}
		seen[effort] = struct{}{}
		reasoning = append(reasoning, effort)
	}
	d.ReasoningEfforts = reasoning
	if len(reasoning) > 0 {
		d.SupportsReasoningEffort = true
	}
	if d.InferenceIdleTimeout <= 0 && d.InferenceIdleTimeoutSecs > 0 {
		d.InferenceIdleTimeout = time.Duration(d.InferenceIdleTimeoutSecs) * time.Second
	}
	if d.InferenceIdleTimeoutSecs == 0 && d.InferenceIdleTimeout > 0 {
		d.InferenceIdleTimeoutSecs = uint64(d.InferenceIdleTimeout / time.Second)
	}
	return d
}

// NormalizeReasoningEffort applies the proxy's deliberately forgiving wire
// rule. A caller-provided effort is never dropped: none, an unknown token, or
// a value unsupported by this account's descriptor is sent as low.
func NormalizeReasoningEffort(input string, descriptor ModelDescriptor) string {
	effort := strings.ToLower(strings.TrimSpace(input))
	if effort == "none" || !isCanonicalReasoningEffort(effort) {
		return "low"
	}
	descriptor = descriptor.Normalize()
	if !descriptor.SupportsReasoningEffort {
		return "low"
	}
	if len(descriptor.ReasoningEfforts) == 0 {
		return effort
	}
	for _, supported := range descriptor.ReasoningEfforts {
		if supported == effort {
			return effort
		}
	}
	return "low"
}

func isCanonicalReasoningEffort(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

// ParseDescriptor parses one /models or /models-v2 item. Both camelCase and
// snake_case spellings used by CLI 0.2.102 are accepted. Unsupported or
// malformed optional fields are silently ignored.
func ParseDescriptor(value any) (ModelDescriptor, bool) {
	obj, ok := object(value)
	if !ok {
		return ModelDescriptor{}, false
	}
	meta, _ := object(obj["_meta"])
	read := func(keys ...string) any {
		for _, key := range keys {
			if value, exists := obj[key]; exists {
				return value
			}
		}
		for _, key := range keys {
			if value, exists := meta[key]; exists {
				return value
			}
		}
		return nil
	}
	id := text(read("id"))
	wire := firstText(read("model"), read("modelId"), read("model_id"), id)
	if id == "" {
		id = wire
	}
	if id == "" || wire == "" {
		return ModelDescriptor{}, false
	}
	d := ModelDescriptor{
		ID:                      id,
		WireModel:               wire,
		Backend:                 Backend(text(read("apiBackend", "api_backend"))),
		ContextWindow:           unsigned(read("contextWindow", "context_window", "totalContextTokens")),
		MaxCompletionTokens:     uint32Bounded(read("maxCompletionTokens", "max_completion_tokens")),
		SupportsReasoningEffort: boolean(read("supportsReasoningEffort", "supports_reasoning_effort")),
		ReasoningEfforts:        reasoningEfforts(read("reasoningEfforts", "reasoning_efforts")),
		SupportsBackendSearch:   boolean(read("supportsBackendSearch", "supports_backend_search")),
		StreamToolCalls:         boolean(read("streamToolCalls", "stream_tool_calls")),
		Hidden:                  boolean(read("hidden")),
		SupportedInAPI:          true,
		InferenceIdleTimeoutSecs: unsigned(read(
			"inferenceIdleTimeoutSecs", "inference_idle_timeout_secs",
		)),
		MaxRetries: optionalUint32(read("maxRetries", "max_retries")),
		AutoCompactThreshold: optionalPercent(read(
			"autoCompactThresholdPercent", "auto_compact_threshold_percent",
		)),
	}
	if supported, ok := read("supportedInApi", "supported_in_api").(bool); ok {
		d.SupportedInAPI = supported
	}
	if value, ok := fixedUint64(read("compactionAtTokens", "compaction_at_tokens"), d.ContextWindow, d.AutoCompactThreshold); ok {
		d.CompactionAtTokens = &value
	}
	if value, ok := fixedUint8(read("compactionsRemaining", "compactions_remaining", "sendCompactionsRemaining")); ok {
		d.CompactionsRemaining = &value
	}
	return d.Normalize(), true
}

// AggregatedModel is the conservative public view of descriptors advertised
// by every usable account that exposes an ID.
type AggregatedModel struct {
	ID                      string    `json:"id"`
	Created                 int64     `json:"created"`
	APIBackends             []Backend `json:"api_backends"`
	ContextWindow           uint64    `json:"context_window,omitempty"`
	MaxCompletionTokens     uint32    `json:"max_completion_tokens,omitempty"`
	ReasoningEfforts        []string  `json:"reasoning_efforts"`
	SupportsBackendSearch   bool      `json:"supports_backend_search"`
	StreamToolCalls         bool      `json:"stream_tool_calls"`
	SupportsReasoningEffort bool      `json:"supports_reasoning_effort"`
}

// Aggregate groups descriptors by public model ID. Backends are unioned,
// positive numeric limits use the minimum, effort values are intersected,
// and booleans require support from every candidate account.
func Aggregate(descriptors []ModelDescriptor) []AggregatedModel {
	groups := make(map[string][]ModelDescriptor)
	for _, descriptor := range descriptors {
		descriptor = descriptor.Normalize()
		if descriptor.ID != "" && !descriptor.Hidden {
			groups[descriptor.ID] = append(groups[descriptor.ID], descriptor)
		}
	}
	result := make([]AggregatedModel, 0, len(groups))
	for id, candidates := range groups {
		item := AggregatedModel{
			ID:                      id,
			SupportsBackendSearch:   true,
			StreamToolCalls:         true,
			SupportsReasoningEffort: true,
		}
		backends := map[Backend]struct{}{}
		var efforts map[string]struct{}
		for index, descriptor := range candidates {
			backends[descriptor.Backend] = struct{}{}
			item.ContextWindow = minimumPositive64(item.ContextWindow, descriptor.ContextWindow)
			item.MaxCompletionTokens = minimumPositive32(item.MaxCompletionTokens, descriptor.MaxCompletionTokens)
			if item.Created == 0 || descriptor.Created > 0 && descriptor.Created < item.Created {
				item.Created = descriptor.Created
			}
			item.SupportsBackendSearch = item.SupportsBackendSearch && descriptor.SupportsBackendSearch
			item.StreamToolCalls = item.StreamToolCalls && descriptor.StreamToolCalls
			item.SupportsReasoningEffort = item.SupportsReasoningEffort && descriptor.SupportsReasoningEffort
			candidateEfforts := supportedEffortSet(descriptor)
			if index == 0 {
				efforts = candidateEfforts
			} else {
				for effort := range efforts {
					if _, exists := candidateEfforts[effort]; !exists {
						delete(efforts, effort)
					}
				}
			}
		}
		for backend := range backends {
			item.APIBackends = append(item.APIBackends, backend)
		}
		sort.Slice(item.APIBackends, func(i, j int) bool { return item.APIBackends[i] < item.APIBackends[j] })
		for effort := range efforts {
			item.ReasoningEfforts = append(item.ReasoningEfforts, effort)
		}
		sort.Strings(item.ReasoningEfforts)
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func supportedEffortSet(descriptor ModelDescriptor) map[string]struct{} {
	result := map[string]struct{}{}
	if !descriptor.SupportsReasoningEffort {
		return result
	}
	if len(descriptor.ReasoningEfforts) == 0 {
		for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
			result[effort] = struct{}{}
		}
		return result
	}
	for _, effort := range descriptor.ReasoningEfforts {
		result[effort] = struct{}{}
	}
	return result
}

func object(value any) (map[string]any, bool) {
	switch value := value.(type) {
	case map[string]any:
		return value, true
	case json.RawMessage:
		var result map[string]any
		if json.Unmarshal(value, &result) == nil {
			return result, true
		}
	case []byte:
		var result map[string]any
		if json.Unmarshal(value, &result) == nil {
			return result, true
		}
	}
	return nil, false
}

func text(value any) string {
	result, _ := value.(string)
	return strings.TrimSpace(result)
}

func firstText(values ...any) string {
	for _, value := range values {
		if result := text(value); result != "" {
			return result
		}
	}
	return ""
}

func boolean(value any) bool {
	result, _ := value.(bool)
	return result
}

func unsigned(value any) uint64 {
	switch value := value.(type) {
	case float64:
		if value > 0 && value <= math.MaxUint64 && math.Trunc(value) == value {
			return uint64(value)
		}
	case json.Number:
		if result, err := value.Int64(); err == nil && result > 0 {
			return uint64(result)
		}
	case int:
		if value > 0 {
			return uint64(value)
		}
	case int64:
		if value > 0 {
			return uint64(value)
		}
	case uint64:
		return value
	}
	return 0
}

func uint32Bounded(value any) uint32 {
	result := unsigned(value)
	if result > uint64(^uint32(0)) {
		return 0
	}
	return uint32(result)
}

func optionalUint32(value any) *uint32 {
	result, ok := nonnegativeInteger(value)
	if !ok || result > math.MaxUint32 {
		return nil
	}
	bounded := uint32(result)
	return &bounded
}

func optionalPercent(value any) *uint8 {
	result, ok := nonnegativeInteger(value)
	if !ok || result > 100 {
		return nil
	}
	bounded := uint8(result)
	return &bounded
}

func reasoningEfforts(value any) []string {
	values, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return strings
		}
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if effort := text(value); effort != "" {
			result = append(result, effort)
			continue
		}
		if option, ok := object(value); ok {
			if effort := text(option["value"]); effort != "" {
				result = append(result, effort)
			}
		}
	}
	return result
}

func fixedUint64(value any, contextWindow uint64, threshold *uint8) (uint64, bool) {
	if enabled, ok := value.(bool); ok {
		if !enabled || contextWindow == 0 {
			return 0, false
		}
		resolvedThreshold := uint8(85)
		if threshold != nil {
			resolvedThreshold = *threshold
		}
		return contextWindow * uint64(resolvedThreshold) / 100, true
	}
	return nonnegativeInteger(value)
}

func fixedUint8(value any) (uint8, bool) {
	if dynamic, ok := value.(bool); ok {
		if !dynamic {
			return 0, false
		}
		return 1, true
	}
	result, ok := nonnegativeInteger(value)
	if !ok {
		return 0, false
	}
	if result > 255 {
		return 0, false
	}
	return uint8(result), true
}

func nonnegativeInteger(value any) (uint64, bool) {
	switch value := value.(type) {
	case float64:
		if value >= 0 && value <= math.MaxUint64 && math.Trunc(value) == value {
			return uint64(value), true
		}
	case json.Number:
		if result, err := value.Int64(); err == nil && result >= 0 {
			return uint64(result), true
		}
	case int:
		if value >= 0 {
			return uint64(value), true
		}
	case int64:
		if value >= 0 {
			return uint64(value), true
		}
	case uint:
		return uint64(value), true
	case uint64:
		return value, true
	}
	return 0, false
}

func minimumPositive64(current, candidate uint64) uint64 {
	if current == 0 || candidate > 0 && candidate < current {
		return candidate
	}
	return current
}

func minimumPositive32(current, candidate uint32) uint32 {
	if current == 0 || candidate > 0 && candidate < current {
		return candidate
	}
	return current
}
