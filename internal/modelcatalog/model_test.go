package modelcatalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		descriptor ModelDescriptor
		want       string
	}{
		{"none is always low", "none", ModelDescriptor{SupportsReasoningEffort: true, ReasoningEfforts: []string{"none"}}, "low"},
		{"unknown", "future", ModelDescriptor{SupportsReasoningEffort: true}, "low"},
		{"trim and case", "  XHIGH ", ModelDescriptor{SupportsReasoningEffort: true, ReasoningEfforts: []string{"xhigh"}}, "xhigh"},
		{"supported", "medium", ModelDescriptor{SupportsReasoningEffort: true, ReasoningEfforts: []string{"low", "medium"}}, "medium"},
		{"known unsupported", "high", ModelDescriptor{SupportsReasoningEffort: true, ReasoningEfforts: []string{"low"}}, "low"},
		{"reasoning disabled", "high", ModelDescriptor{}, "low"},
		{"low absent still low", "high", ModelDescriptor{SupportsReasoningEffort: true, ReasoningEfforts: []string{"medium"}}, "low"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := NormalizeReasoningEffort(test.input, test.descriptor); got != test.want {
				t.Fatalf("NormalizeReasoningEffort() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseDescriptor(t *testing.T) {
	descriptor, ok := ParseDescriptor(map[string]any{
		"id": "public-id", "model": "wire-slug", "apiBackend": "responses",
		"contextWindow": float64(500000), "max_completion_tokens": float64(32768),
		"reasoningEfforts":      []any{"low", map[string]any{"value": "xhigh"}, "future"},
		"supportsBackendSearch": true, "streamToolCalls": true,
	})
	if !ok {
		t.Fatal("ParseDescriptor rejected valid item")
	}
	if descriptor.ID != "public-id" || descriptor.WireModel != "wire-slug" || descriptor.Backend != BackendResponses {
		t.Fatalf("unexpected identity/routing: %#v", descriptor)
	}
	if !descriptor.SupportsReasoningEffort || !reflect.DeepEqual(descriptor.ReasoningEfforts, []string{"low", "xhigh"}) {
		t.Fatalf("unexpected reasoning metadata: %#v", descriptor)
	}
}

func TestParseDescriptorIgnoresInvalidCompactionTransportValues(t *testing.T) {
	descriptor, ok := ParseDescriptor(map[string]any{
		"id": "grok", "model": "grok",
		"compactionsRemaining": "not-a-number", "compactionAtTokens": false,
	})
	if !ok {
		t.Fatal("ParseDescriptor rejected descriptor")
	}
	if descriptor.CompactionsRemaining != nil || descriptor.CompactionAtTokens != nil {
		t.Fatalf("invalid compaction values survived: %#v", descriptor)
	}
}

func TestParseDescriptorPreservesExplicitZeroTransportValues(t *testing.T) {
	t.Run("derived compaction at zero percent", func(t *testing.T) {
		descriptor, ok := ParseDescriptor(map[string]any{
			"id": "grok", "model": "grok", "contextWindow": float64(500000),
			"maxRetries": float64(0), "autoCompactThresholdPercent": float64(0),
			"compactionAtTokens": true,
		})
		if !ok {
			t.Fatal("ParseDescriptor rejected descriptor")
		}
		if descriptor.MaxRetries == nil || *descriptor.MaxRetries != 0 {
			t.Fatalf("max retries = %#v, want explicit zero", descriptor.MaxRetries)
		}
		if descriptor.AutoCompactThreshold == nil || *descriptor.AutoCompactThreshold != 0 {
			t.Fatalf("auto compact threshold = %#v, want explicit zero", descriptor.AutoCompactThreshold)
		}
		if descriptor.CompactionAtTokens == nil || *descriptor.CompactionAtTokens != 0 {
			t.Fatalf("compaction at tokens = %#v, want derived zero", descriptor.CompactionAtTokens)
		}
		payload, err := json.Marshal(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"max_retries":0`, `"auto_compact_threshold_percent":0`, `"compaction_at_tokens":0`} {
			if !strings.Contains(string(payload), field) {
				t.Fatalf("serialized descriptor %s does not preserve %s", payload, field)
			}
		}
	})

	t.Run("fixed zero token count", func(t *testing.T) {
		descriptor, ok := ParseDescriptor(map[string]any{
			"id": "grok", "model": "grok", "compactionAtTokens": float64(0),
		})
		if !ok {
			t.Fatal("ParseDescriptor rejected descriptor")
		}
		if descriptor.CompactionAtTokens == nil || *descriptor.CompactionAtTokens != 0 {
			t.Fatalf("compaction at tokens = %#v, want fixed zero", descriptor.CompactionAtTokens)
		}
		if descriptor.MaxRetries != nil || descriptor.AutoCompactThreshold != nil {
			t.Fatalf("missing values were confused with zero: %#v", descriptor)
		}
	})
}

func TestAggregateIsConservative(t *testing.T) {
	got := Aggregate([]ModelDescriptor{
		{ID: "grok", Backend: BackendResponses, ContextWindow: 500000, MaxCompletionTokens: 32000, SupportsReasoningEffort: true, ReasoningEfforts: []string{"low", "high"}, SupportsBackendSearch: true, StreamToolCalls: true, Created: 20},
		{ID: "grok", Backend: BackendMessages, ContextWindow: 300000, MaxCompletionTokens: 16000, SupportsReasoningEffort: true, ReasoningEfforts: []string{"low", "medium"}, SupportsBackendSearch: false, StreamToolCalls: true, Created: 10},
	})
	if len(got) != 1 {
		t.Fatalf("Aggregate() len = %d", len(got))
	}
	item := got[0]
	if item.ContextWindow != 300000 || item.MaxCompletionTokens != 16000 || item.Created != 10 {
		t.Fatalf("unexpected numeric aggregation: %#v", item)
	}
	if !reflect.DeepEqual(item.ReasoningEfforts, []string{"low"}) || item.SupportsBackendSearch || !item.StreamToolCalls {
		t.Fatalf("unexpected capability aggregation: %#v", item)
	}
}
