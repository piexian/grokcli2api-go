package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func TestContinuityPersistsOnlyHashedHardBindings(t *testing.T) {
	dir := t.TempDir()
	store := newContinuityStore(dir, time.Hour, 32)
	affinity := auth.Affinity{Key: "session:downstream-secret", Mode: auth.AffinityHard}
	key := continuityKey("tenant-secret", affinity, "grok-test")
	store.Bind(key, affinity, "account-1", "grok-test", modelcatalog.BackendResponses, "upstream-session", 1)
	store.Close()

	payload, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"downstream-secret", "tenant-secret"} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("affinity file contains plaintext %q: %s", secret, payload)
		}
	}
	if !strings.Contains(string(payload), "upstream-session") {
		t.Fatalf("affinity file did not persist upstream session: %s", payload)
	}

	reloaded := newContinuityStore(dir, time.Hour, 32)
	defer reloaded.Close()
	gotKey, binding, ok := reloaded.Lookup("tenant-secret", affinity, "grok-test")
	if !ok || gotKey != key || binding.AccountID != "account-1" || binding.Backend != modelcatalog.BackendResponses {
		t.Fatalf("reloaded binding = key %q, %#v, %v", gotKey, binding, ok)
	}
	_, turn, ok := reloaded.ReserveTurn(key)
	if !ok || turn != 1 {
		t.Fatalf("reserved turn = %d, %v; want 1, true", turn, ok)
	}
	_, turn, ok = reloaded.ReserveTurn(key)
	if !ok || turn != 2 {
		t.Fatalf("second reserved turn = %d, %v; want 2, true", turn, ok)
	}
}

func TestContinuityIgnoresCorruptOrExpiredState(t *testing.T) {
	dir := t.TempDir()
	path := dir + string(os.PathSeparator) + affinityStateFileName
	if err := os.WriteFile(path, []byte(`{"version":1,"hard_affinities":`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newContinuityStore(dir, 10*time.Millisecond, 4)
	defer store.Close()
	affinity := auth.Affinity{Key: "previous:resp", Mode: auth.AffinityHard}
	if _, _, ok := store.Lookup("tenant", affinity, "model"); ok {
		t.Fatal("corrupt affinity state unexpectedly restored a binding")
	}
	key := continuityKey("tenant", affinity, "model")
	store.Bind(key, affinity, "account", "model", modelcatalog.BackendResponses, "session", 1)
	time.Sleep(15 * time.Millisecond)
	if _, _, ok := store.Lookup("tenant", affinity, "model"); ok {
		t.Fatal("expired affinity binding remained available")
	}
}

func TestContinuityBindsOpaqueStateTokensWithoutPersistingPlaintext(t *testing.T) {
	dir := t.TempDir()
	store := newContinuityStore(dir, time.Hour, 8)
	store.BindStateToken("tenant", "opaque-signature", "account", "model", modelcatalog.BackendMessages, "session", 1)
	store.Close()
	payload, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "opaque-signature") {
		t.Fatalf("state token was persisted in plaintext: %s", payload)
	}
	reloaded := newContinuityStore(dir, time.Hour, 8)
	defer reloaded.Close()
	affinity := auth.Affinity{Key: "signature:opaque-signature", Mode: auth.AffinityHard}
	_, binding, ok := reloaded.Lookup("tenant", affinity, "model")
	if !ok || binding.AccountID != "account" || binding.Backend != modelcatalog.BackendMessages {
		t.Fatalf("binding=%#v ok=%v", binding, ok)
	}
}

func TestResponseStateTokensFindsMessagesAndResponsesState(t *testing.T) {
	tokens := responseStateTokens(map[string]any{"output": []any{
		map[string]any{"type": "reasoning", "encrypted_content": "enc"},
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "thinking", "signature": "sig"}}},
		map[string]any{"type": "reasoning", "encrypted_content": "enc"},
	}})
	if len(tokens) != 2 || tokens[0] != "enc" || tokens[1] != "sig" {
		t.Fatalf("tokens=%#v", tokens)
	}
}

func TestStreamStateCollectorReassemblesSignatureDeltas(t *testing.T) {
	collector := newStreamStateCollector()
	collector.Observe(grok.SSEEvent{Event: "content_block_delta", Data: mustJSON(map[string]any{
		"type": "content_block_delta", "index": 2,
		"delta": map[string]any{"type": "signature_delta", "signature": "opaque-"},
	})})
	collector.Observe(grok.SSEEvent{Event: "content_block_delta", Data: mustJSON(map[string]any{
		"type": "content_block_delta", "index": 2,
		"delta": map[string]any{"type": "signature_delta", "signature": "signature"},
	})})
	collector.Observe(grok.SSEEvent{Event: "content_block_stop", Data: mustJSON(map[string]any{
		"type": "content_block_stop", "index": 2,
	})})
	collector.Observe(grok.SSEEvent{Event: "response.output_item.done", Data: mustJSON(map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{"type": "reasoning", "encrypted_content": "encrypted-state"},
	})})

	tokens := collector.Tokens()
	if len(tokens) != 2 || tokens[0] != "opaque-signature" || tokens[1] != "encrypted-state" {
		t.Fatalf("tokens=%#v", tokens)
	}
}

func TestCommitStreamContinuityBindsStateOnlyAfterSuccess(t *testing.T) {
	store := newContinuityStore(t.TempDir(), time.Hour, 16)
	defer store.Close()
	server := &Server{continuity: store}
	turn := uint64(4)
	stream := &grok.InferenceStream{
		Attempt:   inference.RenderedAttempt{Backend: modelcatalog.BackendMessages},
		AccountID: "account-a",
		Identity:  grok.RequestIdentity{SessionID: "upstream-session", TurnIndex: &turn},
	}
	execution := inferenceExecution{tenant: "tenant-a", model: "grok-model"}

	failed := &backendStreamAdapter{terminal: true, success: false}
	server.commitStreamContinuity(execution, stream, failed, inference.ProtocolMessages, []string{"failed-signature"})
	if _, _, ok := store.Lookup("tenant-a", auth.Affinity{Key: "signature:failed-signature", Mode: auth.AffinityHard}, "grok-model"); ok {
		t.Fatal("failed stream created a state binding")
	}

	succeeded := &backendStreamAdapter{terminal: true, success: true}
	server.commitStreamContinuity(execution, stream, succeeded, inference.ProtocolMessages, []string{"good-signature"})
	affinity := auth.Affinity{Key: "signature:good-signature", Mode: auth.AffinityHard}
	_, binding, ok := store.Lookup("tenant-a", affinity, "grok-model")
	if !ok || binding.AccountID != "account-a" || binding.NextTurn != 5 {
		t.Fatalf("binding=%#v ok=%v", binding, ok)
	}
	if _, _, ok := store.Lookup("tenant-b", affinity, "grok-model"); ok {
		t.Fatal("state binding crossed tenant namespace")
	}
}
