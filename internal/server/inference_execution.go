package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
	"github.com/Futureppo/grokcli2api-go/internal/openai"
)

type continuityRequestError struct {
	Status  int
	Code    string
	Message string
}

func (e *continuityRequestError) Error() string { return e.Message }

type inferenceExecution struct {
	tenant          string
	model           string
	plan            *inference.RequestPlan
	affinity        auth.Affinity
	pinned          string
	expectedBackend modelcatalog.Backend
	identity        grok.RequestIdentity
	pendingBindings []pendingContinuityBinding
}

type pendingContinuityBinding struct {
	key      string
	affinity auth.Affinity
	state    *inference.StateHandle
}

type continuityLookup struct {
	handle   inference.StateHandle
	affinity auth.Affinity
	key      string
	binding  continuityBinding
	found    bool
}

func (s *Server) prepareInferenceExecution(r *http.Request, body map[string]any, plan *inference.RequestPlan) (inferenceExecution, error) {
	tenant := tenantFromContext(r.Context())
	execution := inferenceExecution{tenant: tenant, model: plan.Model(), plan: plan, affinity: requestSoftAffinity(r, body)}
	execution.identity = grok.RequestIdentity{RequestID: grok.NewID(), SessionID: newLogicalSession()}
	execution.identity.ConversationID = execution.identity.SessionID
	turn := uint64(0)
	execution.identity.TurnIndex = &turn

	var session continuityLookup
	if value := strings.TrimSpace(r.Header.Get("X-Grok-Session-ID")); value != "" {
		session.affinity = auth.Affinity{Tenant: tenant, Key: "session:" + value, Mode: auth.AffinityHard}
		session.key, session.binding, session.found = s.continuity.Lookup(tenant, session.affinity, plan.Model())
	}

	state := make([]continuityLookup, 0, len(plan.StateHandles()))
	unknownOpaque := make([]inference.StateHandle, 0)
	for _, handle := range plan.StateHandles() {
		affinity := stateHandleAffinity(tenant, handle)
		key, binding, found := s.continuity.Lookup(tenant, affinity, plan.Model())
		if handle.Kind == inference.StateOpaqueToken && !found {
			unknownOpaque = append(unknownOpaque, handle)
			continue
		}
		state = append(state, continuityLookup{handle: handle, affinity: affinity, key: key, binding: binding, found: found})
	}
	if len(unknownOpaque) > 0 {
		plan = plan.WithoutOpaqueState(unknownOpaque)
		execution.plan = plan
		slog.Warn("unknown opaque upstream state dropped", "model", plan.Model(), "protocol", plan.Protocol(), "count", len(unknownOpaque))
	}

	// An existing explicit session is authoritative. Only state tokens which
	// that account's renderer will actually retain need ownership validation;
	// tokens removed by the renderer follow the silent-cleaning rule.
	if session.found {
		attempt, err := s.renderForBinding(plan, session.binding)
		if err != nil {
			return inferenceExecution{}, err
		}
		return s.prepareBoundExecution(execution, session, session, state, *attempt)
	}

	// Otherwise, the first locally owned state handle whose bound renderer can
	// retain it selects the route. A later handle is still validated below, so
	// placing an owned token before a foreign token cannot bypass isolation.
	for _, candidate := range state {
		if !candidate.found {
			continue
		}
		attempt, err := s.renderForBinding(plan, candidate.binding)
		if err != nil {
			return inferenceExecution{}, err
		}
		if renderedStateHandleSet(*attempt)[stateHandleKey(candidate.handle)] {
			return s.prepareBoundExecution(execution, candidate, session, state, *attempt)
		}
	}

	// With no known binding, fail closed only for opaque state a viable route
	// could put on the wire. If every candidate renderer removes it, start a
	// fresh conversation exactly as required by the silent-cleaning policy.
	potential := s.planPreservedStateHandles(plan)
	for _, item := range state {
		if !potential[stateHandleKey(item.handle)] {
			continue
		}
		if item.found {
			return inferenceExecution{}, upstreamStateUnavailable()
		}
		if tenant != "public" {
			return inferenceExecution{}, unknownStateHandle(item.handle)
		}
	}

	if session.affinity.Key != "" {
		execution.affinity = session.affinity
		execution.pendingBindings = append(execution.pendingBindings, pendingContinuityBinding{key: session.key, affinity: session.affinity})
		return execution, nil
	}

	// The shared public namespace intentionally cannot isolate callers. Retain
	// its legacy best-effort behavior for state which a candidate can carry.
	if tenant == "public" {
		for index := range state {
			if potential[stateHandleKey(state[index].handle)] {
				execution.affinity = state[index].affinity
				handle := state[index].handle
				execution.pendingBindings = append(execution.pendingBindings, pendingContinuityBinding{
					key: state[index].key, affinity: state[index].affinity, state: &handle,
				})
				break
			}
		}
	}
	return execution, nil
}

func (s *Server) prepareBoundExecution(execution inferenceExecution, selected, session continuityLookup, state []continuityLookup, attempt inference.RenderedAttempt) (inferenceExecution, error) {
	preserved := renderedStateHandleSet(attempt)
	bindings := []continuityLookup{selected}
	for _, item := range state {
		if !preserved[stateHandleKey(item.handle)] {
			continue
		}
		if !item.found {
			if execution.tenant != "public" {
				return inferenceExecution{}, unknownStateHandle(item.handle)
			}
			continue
		}
		if !sameContinuityRoute(selected.binding, item.binding) {
			return inferenceExecution{}, upstreamStateUnavailable()
		}
		bindings = append(bindings, item)
	}

	// Explicit sessions are client-created aliases. A new alias may accompany
	// an already-owned opaque token and is bound only after a successful
	// terminal response.
	if session.affinity.Key != "" && !session.found {
		execution.pendingBindings = append(execution.pendingBindings, pendingContinuityBinding{key: session.key, affinity: session.affinity})
	}
	seenBindings := make(map[string]struct{}, len(bindings))
	for index := range bindings {
		item := bindings[index]
		if item.key == "" {
			continue
		}
		if _, duplicate := seenBindings[item.key]; duplicate {
			continue
		}
		seenBindings[item.key] = struct{}{}
		pending := pendingContinuityBinding{key: item.key, affinity: item.affinity}
		if item.handle.Value != "" {
			handle := item.handle
			pending.state = &handle
		}
		execution.pendingBindings = append(execution.pendingBindings, pending)
	}

	reserve := bindings[0]
	for _, item := range bindings[1:] {
		if item.binding.NextTurn > reserve.binding.NextTurn {
			reserve = item
		}
	}
	reserved, reservedTurn, ok := s.continuity.ReserveTurn(reserve.key)
	if !ok || !sameContinuityRoute(selected.binding, reserved) {
		return inferenceExecution{}, upstreamStateUnavailable()
	}
	execution.affinity = selected.affinity
	execution.pinned = selected.binding.AccountID
	execution.expectedBackend = selected.binding.Backend
	execution.identity.SessionID = selected.binding.SessionID
	execution.identity.ConversationID = selected.binding.SessionID
	execution.identity.TurnIndex = &reservedTurn
	return execution, nil
}

func (s *Server) renderForBinding(plan *inference.RequestPlan, binding continuityBinding) (*inference.RenderedAttempt, error) {
	descriptor, ok := s.pool.AccountDescriptor(binding.AccountID, plan.Model())
	if !ok {
		descriptor = provisionalModelDescriptor(plan.Protocol(), plan.Model())
	}
	if descriptor.Backend != binding.Backend {
		return nil, upstreamStateUnavailable()
	}
	return plan.Render(descriptor)
}

func (s *Server) planPreservedStateHandles(plan *inference.RequestPlan) map[string]bool {
	preserved := make(map[string]bool)
	if plan == nil || !plan.HasState() {
		return preserved
	}
	for _, model := range s.pool.AggregatedModels() {
		if model.ID != plan.Model() {
			continue
		}
		for _, backend := range model.APIBackends {
			descriptor := modelcatalog.ModelDescriptor{
				ID: plan.Model(), WireModel: plan.Model(), Backend: backend,
				SupportsReasoningEffort: model.SupportsReasoningEffort,
				ReasoningEfforts:        model.ReasoningEfforts,
			}
			if attempt, err := plan.Render(descriptor); err == nil {
				for key := range renderedStateHandleSet(*attempt) {
					preserved[key] = true
				}
			}
		}
	}
	if s.pool.HasProvisionalModel(plan.Model()) {
		if attempt, err := plan.Render(provisionalModelDescriptor(plan.Protocol(), plan.Model())); err == nil {
			for key := range renderedStateHandleSet(*attempt) {
				preserved[key] = true
			}
		}
	}
	return preserved
}

func provisionalModelDescriptor(_ inference.Protocol, model string) modelcatalog.ModelDescriptor {
	return modelcatalog.ModelDescriptor{
		ID: model, WireModel: model, Backend: modelcatalog.BackendChatCompletions, SupportedInAPI: true,
	}
}

func stateHandleAffinity(tenant string, handle inference.StateHandle) auth.Affinity {
	prefix := "signature:"
	if handle.Kind == inference.StatePreviousResponse {
		prefix = "previous:"
	}
	return auth.Affinity{Tenant: tenant, Key: prefix + handle.Value, Mode: auth.AffinityHard}
}

func stateHandleKey(handle inference.StateHandle) string {
	return string(handle.Kind) + "\x00" + strings.TrimSpace(handle.Value)
}

func renderedStateHandleSet(attempt inference.RenderedAttempt) map[string]bool {
	result := make(map[string]bool)
	add := func(kind inference.StateHandleKind, value string) {
		handle := inference.StateHandle{Kind: kind, Value: strings.TrimSpace(value)}
		if handle.Value != "" {
			result[stateHandleKey(handle)] = true
		}
	}
	if value, ok := attempt.Body["previous_response_id"].(string); ok {
		add(inference.StatePreviousResponse, value)
	}
	switch attempt.Backend {
	case modelcatalog.BackendResponses:
		input, _ := attempt.Body["input"].([]any)
		for _, rawItem := range input {
			item, _ := rawItem.(map[string]any)
			if strings.EqualFold(strings.TrimSpace(stringAt(item, "type")), "reasoning") {
				add(inference.StateOpaqueToken, stringAt(item, "encrypted_content"))
			}
		}
	case modelcatalog.BackendMessages:
		messages, _ := attempt.Body["messages"].([]any)
		for _, rawMessage := range messages {
			message, _ := rawMessage.(map[string]any)
			content, _ := message["content"].([]any)
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				kind := strings.TrimSpace(stringAt(block, "type"))
				if strings.EqualFold(kind, "thinking") || strings.EqualFold(kind, "redacted_thinking") {
					add(inference.StateOpaqueToken, stringAt(block, "signature"))
				}
			}
		}
	}
	return result
}

func sameContinuityRoute(left, right continuityBinding) bool {
	return left.AccountID != "" && left.AccountID == right.AccountID &&
		left.Model == right.Model && left.Backend == right.Backend && left.SessionID == right.SessionID
}

func upstreamStateUnavailable() error {
	return &continuityRequestError{
		Status: http.StatusServiceUnavailable, Code: "upstream_state_unavailable",
		Message: "upstream state is unavailable for the bound account",
	}
}

func unknownStateHandle(handle inference.StateHandle) error {
	if handle.Kind == inference.StatePreviousResponse {
		return &continuityRequestError{
			Status: http.StatusNotFound, Code: "previous_response_not_found",
			Message: "previous_response_id is unknown for this API tenant",
		}
	}
	return &continuityRequestError{
		Status: http.StatusNotFound, Code: "upstream_state_not_found",
		Message: "opaque upstream state is unknown for this API tenant",
	}
}

func (s *Server) executeNonStreaming(w http.ResponseWriter, r *http.Request, body map[string]any, plan *inference.RequestPlan, compat *openai.ResponsesCompatibility) {
	execution, err := s.prepareInferenceExecution(r, body, plan)
	if err != nil {
		s.writePlanError(w, plan.Protocol(), err)
		return
	}
	plan = execution.plan
	result, err := s.client.DoInference(r.Context(), plan, grok.InferenceOptions{
		Affinity: execution.affinity, PinnedAccount: execution.pinned, ExpectedBackend: execution.expectedBackend, Identity: execution.identity,
	})
	if err != nil {
		var requestErr *inference.RequestError
		if errors.As(err, &requestErr) || errors.Is(err, inference.ErrNoRepresentableInput) {
			s.writePlanError(w, plan.Protocol(), err)
			return
		}
		if plan.Protocol() == inference.ProtocolMessages {
			s.writeAnthropicClientError(w, err)
		} else {
			s.writeClientError(w, err)
		}
		return
	}
	copyModelHeaders(w.Header(), result.Headers)
	anthropicOptions := responseOptionsFromMessages(body)
	payload := adaptBackendResponse(result.Attempt.Adapter, result.Payload, plan.Model(), anthropicOptions)
	if plan.Protocol() == inference.ProtocolResponses {
		if compat != nil {
			payload = compat.NormalizeResponse(payload, plan.Model())
		}
		store, _ := result.Attempt.Body["store"].(bool)
		openai.RememberCompletedResponseWithStoreForTenant(
			openai.DefaultToolReplay, execution.tenant, plan.Model(), payload,
			openai.String(body, "prompt_cache_key", ""), store,
		)
	}
	s.commitContinuity(execution, result.AccountID, result.Attempt, result.Identity, payload, plan.Protocol())
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) executeStreaming(w http.ResponseWriter, r *http.Request, body map[string]any, plan *inference.RequestPlan, compat *openai.ResponsesCompatibility) {
	execution, err := s.prepareInferenceExecution(r, body, plan)
	if err != nil {
		s.writePlanError(w, plan.Protocol(), err)
		return
	}
	plan = execution.plan
	stream, err := s.client.OpenInference(r.Context(), plan, grok.InferenceOptions{
		Affinity: execution.affinity, PinnedAccount: execution.pinned, ExpectedBackend: execution.expectedBackend, Identity: execution.identity,
	})
	if err != nil {
		var requestErr *inference.RequestError
		if errors.As(err, &requestErr) || errors.Is(err, inference.ErrNoRepresentableInput) {
			s.writePlanError(w, plan.Protocol(), err)
			return
		}
		if plan.Protocol() == inference.ProtocolMessages {
			s.writeAnthropicClientError(w, err)
		} else {
			s.writeClientError(w, err)
		}
		return
	}
	defer stream.Close()
	copyModelHeaders(w.Header(), stream.Headers)
	prepareSSE(w)
	flush := flusher(w)
	adapter := newBackendStreamAdapter(stream.Attempt.Adapter, plan.Model(), responseOptionsFromMessages(body))
	store, _ := stream.Attempt.Body["store"].(bool)
	replay := &streamToolReplayState{
		model: plan.Model(), promptCacheKey: openai.String(body, "prompt_cache_key", ""),
		store: store, tenant: execution.tenant,
	}
	state := newStreamStateCollector()
	emit := func(outgoing []grok.SSEEvent) bool {
		for _, translated := range outgoing {
			translatedEvents := []grok.SSEEvent{translated}
			if compat != nil {
				compatEvents, translateErr := compat.TranslateStream(translated.Event, translated.Data)
				if translateErr != nil {
					for _, failure := range adapter.encodeError(translateErr.Error(), "compatibility_error") {
						_ = writeRawSSE(w, failure)
					}
					flush()
					return false
				}
				translatedEvents = translatedEvents[:0]
				for _, compatEvent := range compatEvents {
					translatedEvents = append(translatedEvents, grok.SSEEvent{Event: compatEvent.Event, Data: compatEvent.Data})
				}
			}
			for _, finalEvent := range translatedEvents {
				state.Observe(finalEvent)
				replay.handle(finalEvent.Event, finalEvent.Data)
				if err := writeRawSSE(w, finalEvent); err != nil {
					return false
				}
			}
		}
		return true
	}
	for {
		event, ok, nextErr := stream.Next()
		if nextErr != nil {
			if errors.Is(nextErr, context.Canceled) {
				return
			}
			for _, outgoing := range adapter.encodeError(nextErr.Error(), "upstream_error") {
				_ = writeRawSSE(w, outgoing)
			}
			flush()
			return
		}
		if !ok {
			if !emit(adapter.Finish()) {
				return
			}
			flush()
			if adapter.Success() {
				s.commitStreamContinuity(execution, stream, adapter, plan.Protocol(), state.Tokens())
			}
			return
		}
		outgoing, adaptErr := adapter.Handle(event)
		if adaptErr != nil {
			for _, failure := range adapter.encodeError(adaptErr.Error(), "upstream_stream_error") {
				_ = writeRawSSE(w, failure)
			}
			flush()
			return
		}
		if !emit(outgoing) {
			return
		}
		flush()
		if adapter.Terminal() {
			if adapter.Success() {
				s.commitStreamContinuity(execution, stream, adapter, plan.Protocol(), state.Tokens())
				if plan.Protocol() == inference.ProtocolChatCompletions {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					flush()
				}
			}
			return
		}
	}
}

func (s *Server) commitContinuity(execution inferenceExecution, accountID string, attempt inference.RenderedAttempt, identity grok.RequestIdentity, payload map[string]any, protocol inference.Protocol) {
	nextTurn := uint64(1)
	if identity.TurnIndex != nil {
		nextTurn = *identity.TurnIndex + 1
	}
	s.bindPendingContinuity(execution, accountID, attempt, identity.SessionID, nextTurn)
	if protocol == inference.ProtocolResponses {
		if responseID := responseIdentifier(payload); responseID != "" {
			s.continuity.BindResponse(execution.tenant, responseID, accountID, execution.model, attempt.Backend, identity.SessionID, nextTurn)
		}
	}
	for _, token := range responseStateTokens(payload) {
		s.continuity.BindStateToken(execution.tenant, token, accountID, execution.model, attempt.Backend, identity.SessionID, nextTurn)
	}
}

func (s *Server) commitStreamContinuity(execution inferenceExecution, stream *grok.InferenceStream, adapter *backendStreamAdapter, protocol inference.Protocol, stateTokens []string) {
	if stream == nil || adapter == nil || !adapter.Success() {
		return
	}
	nextTurn := uint64(1)
	if stream.Identity.TurnIndex != nil {
		nextTurn = *stream.Identity.TurnIndex + 1
	}
	s.bindPendingContinuity(execution, stream.AccountID, stream.Attempt, stream.Identity.SessionID, nextTurn)
	if protocol == inference.ProtocolResponses {
		if responseID := adapter.ResponseID(); responseID != "" {
			s.continuity.BindResponse(execution.tenant, responseID, stream.AccountID, execution.model, stream.Attempt.Backend, stream.Identity.SessionID, nextTurn)
		}
	}
	for _, token := range stateTokens {
		s.continuity.BindStateToken(execution.tenant, token, stream.AccountID, execution.model, stream.Attempt.Backend, stream.Identity.SessionID, nextTurn)
	}
}

func (s *Server) bindPendingContinuity(execution inferenceExecution, accountID string, attempt inference.RenderedAttempt, sessionID string, nextTurn uint64) {
	if len(execution.pendingBindings) == 0 {
		return
	}
	preserved := renderedStateHandleSet(attempt)
	for _, pending := range execution.pendingBindings {
		if pending.state != nil && !preserved[stateHandleKey(*pending.state)] {
			continue
		}
		s.continuity.Bind(pending.key, pending.affinity, accountID, execution.model, attempt.Backend, sessionID, nextTurn)
	}
}

// streamStateCollector reconstructs opaque state tokens which were actually
// emitted to the downstream client. Anthropic signature_delta values may be
// split across several SSE events, so binding individual fragments would make
// the next request impossible to route reliably. Tokens are committed only by
// commitStreamContinuity after a successful protocol terminal.
type streamStateCollector struct {
	signatures map[int]*strings.Builder
	seen       map[string]struct{}
	tokens     []string
}

func newStreamStateCollector() *streamStateCollector {
	return &streamStateCollector{
		signatures: make(map[int]*strings.Builder),
		seen:       make(map[string]struct{}),
	}
}

func (c *streamStateCollector) Observe(event grok.SSEEvent) {
	if c == nil || len(event.Data) == 0 || string(event.Data) == "[DONE]" {
		return
	}
	var payload map[string]any
	if json.Unmarshal(event.Data, &payload) != nil {
		return
	}
	for _, token := range responseStateTokens(payload) {
		c.add(token)
	}
	kind := strings.ToLower(strings.TrimSpace(event.Event))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(stringAt(payload, "type")))
	}
	index := int(intAt(payload, "index"))
	switch kind {
	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]any)
		if strings.EqualFold(stringAt(delta, "type"), "signature_delta") {
			builder := c.signatures[index]
			if builder == nil {
				builder = &strings.Builder{}
				c.signatures[index] = builder
			}
			builder.WriteString(stringAt(delta, "signature"))
		}
	case "content_block_stop":
		c.finishSignature(index)
	}
}

func (c *streamStateCollector) Tokens() []string {
	if c == nil {
		return nil
	}
	indices := make([]int, 0, len(c.signatures))
	for index := range c.signatures {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		c.finishSignature(index)
	}
	return append([]string(nil), c.tokens...)
}

func (c *streamStateCollector) finishSignature(index int) {
	builder := c.signatures[index]
	delete(c.signatures, index)
	if builder != nil {
		c.add(builder.String())
	}
}

func (c *streamStateCollector) add(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	if _, exists := c.seen[token]; exists {
		return
	}
	c.seen[token] = struct{}{}
	c.tokens = append(c.tokens, token)
}

func (s *Server) writePlanError(w http.ResponseWriter, protocol inference.Protocol, err error) {
	status, code, message, param := http.StatusBadRequest, "invalid_request", err.Error(), ""
	var continuityErr *continuityRequestError
	if errors.As(err, &continuityErr) {
		status, code, message = continuityErr.Status, continuityErr.Code, continuityErr.Message
	}
	var requestErr *inference.RequestError
	if errors.As(err, &requestErr) {
		param = requestErr.Param
		code = "invalid_value"
	}
	if protocol == inference.ProtocolMessages {
		writeAnthropicError(w, status, message, anthropicErrorType(status))
		return
	}
	writeErrorWithParam(w, status, message, "invalid_request_error", code, param)
}

func copyModelHeaders(dst, src http.Header) {
	for _, name := range []string{"x-grok-context-window", "x-grok-max-completion-tokens", "x-models-etag", "Retry-After", "x-should-retry"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

func responseIdentifier(payload map[string]any) string {
	if id, _ := payload["id"].(string); id != "" {
		return id
	}
	if response, ok := payload["response"].(map[string]any); ok {
		id, _ := response["id"].(string)
		return id
	}
	return ""
}

func responseStateTokens(payload map[string]any) []string {
	seen := make(map[string]struct{})
	var tokens []string
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				visit(item)
			}
		case map[string]any:
			kind := strings.ToLower(strings.TrimSpace(stringAt(value, "type")))
			var token string
			switch kind {
			case "thinking", "redacted_thinking":
				token = stringAt(value, "signature")
			case "reasoning":
				token = stringAt(value, "encrypted_content")
			}
			if token = strings.TrimSpace(token); token != "" {
				if _, exists := seen[token]; !exists {
					seen[token] = struct{}{}
					tokens = append(tokens, token)
				}
			}
			for _, item := range value {
				visit(item)
			}
		}
	}
	visit(payload)
	return tokens
}
