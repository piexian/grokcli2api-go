package grok

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func NewID() string { return randomHex(16) }

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func traceparent() string { return "00-" + randomHex(16) + "-" + randomHex(8) + "-01" }

func platform() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return runtime.GOOS
}

func defaultUserAgent(cfg config.Config) string {
	identifier := strings.TrimSpace(cfg.ClientIdentifier)
	if identifier == "" {
		identifier = "grok-shell"
	}
	if identifier == "grok-shell" {
		return fmt.Sprintf("grok-shell/%s (%s; %s)", cfg.ClientVersion, platform(), runtime.GOARCH)
	}
	return fmt.Sprintf("%s/%s grok-shell/%s (%s; %s)", identifier, cfg.ClientVersion, cfg.ClientVersion, platform(), runtime.GOARCH)
}

func chatUserAgent(cfg config.Config) string {
	return fmt.Sprintf("%s/%s (%s; %s)", cfg.ClientIdentifier, cfg.ClientVersion, platform(), runtime.GOARCH)
}

// RequestIdentity contains the logical identity of one inference request. The
// caller should create it once and reuse it across transport and account
// retries. Empty account-derived fields are filled by Client after acquiring
// the first lease.
type RequestIdentity struct {
	RequestID            string
	SessionID            string
	ConversationID       string
	AgentID              string
	TurnIndex            *uint64
	Model                string
	UserID               string
	DeploymentID         string
	IdleTimeout          time.Duration
	CompactionAtTokens   *uint64
	CompactionsRemaining *uint8
}

type requestIdentityContextKey struct{}

// WithRequestIdentity attaches immutable request identity metadata for
// DoJSON/OpenStream. A pointed-to turn index is copied before storage.
func WithRequestIdentity(ctx context.Context, identity RequestIdentity) context.Context {
	if identity.TurnIndex != nil {
		turn := *identity.TurnIndex
		identity.TurnIndex = &turn
	}
	if identity.CompactionAtTokens != nil {
		value := *identity.CompactionAtTokens
		identity.CompactionAtTokens = &value
	}
	if identity.CompactionsRemaining != nil {
		value := *identity.CompactionsRemaining
		identity.CompactionsRemaining = &value
	}
	return context.WithValue(ctx, requestIdentityContextKey{}, identity)
}

// RequestIdentityFromContext returns a copy of the attached request identity.
func RequestIdentityFromContext(ctx context.Context) (RequestIdentity, bool) {
	identity, ok := ctx.Value(requestIdentityContextKey{}).(RequestIdentity)
	if ok && identity.TurnIndex != nil {
		turn := *identity.TurnIndex
		identity.TurnIndex = &turn
	}
	if ok && identity.CompactionAtTokens != nil {
		value := *identity.CompactionAtTokens
		identity.CompactionAtTokens = &value
	}
	if ok && identity.CompactionsRemaining != nil {
		value := *identity.CompactionsRemaining
		identity.CompactionsRemaining = &value
	}
	return identity, ok
}

// BuildInferenceHeaders implements the Grok CLI inference header contract. It
// never invents logical IDs; callers that need generated IDs should do so once
// before the first attempt.
func BuildInferenceHeaders(cfg config.Config, session auth.Session, identity RequestIdentity, trace bool) http.Header {
	h := make(http.Header)
	h.Set("x-grok-client-version", cfg.ClientVersion)
	h.Set("x-grok-client-identifier", cfg.ClientIdentifier)
	h.Set("x-grok-client-surface", cfg.ClientSurface)
	h.Set("x-grok-client-name", cfg.ClientName)
	mode := strings.ToLower(strings.TrimSpace(cfg.ClientMode))
	if mode == "" {
		mode = "headless"
	}
	h.Set("x-grok-client-mode", mode)
	if !session.IsAPIKey() {
		h.Set("x-xai-token-auth", cfg.TokenAuth)
		h.Set("x-authenticateresponse", "authenticate-response")
	}
	if identity.AgentID != "" {
		h.Set("x-grok-agent-id", identity.AgentID)
	}
	if identity.SessionID != "" {
		h.Set("x-grok-session-id", identity.SessionID)
	}
	if identity.ConversationID != "" {
		h.Set("x-grok-conv-id", identity.ConversationID)
	}
	if identity.RequestID != "" {
		h.Set("x-grok-req-id", identity.RequestID)
	}
	if identity.TurnIndex != nil {
		h.Set("x-grok-turn-idx", strconv.FormatUint(*identity.TurnIndex, 10))
	}
	if identity.Model != "" {
		h.Set("x-grok-model-override", identity.Model)
	}
	if identity.CompactionAtTokens != nil {
		h.Set("x-compaction-at", strconv.FormatUint(*identity.CompactionAtTokens, 10))
	}
	if identity.CompactionsRemaining != nil {
		h.Set("x-compactions-remaining", strconv.FormatUint(uint64(*identity.CompactionsRemaining), 10))
	}
	userID := identity.UserID
	if userID == "" {
		userID = session.UserID
	}
	if userID != "" {
		// x-userid remains required by proxy authentication middleware while
		// x-grok-user-id is the modern inference attribution header.
		h.Set("x-userid", userID)
		h.Set("x-grok-user-id", userID)
	}
	deploymentID := identity.DeploymentID
	if deploymentID == "" {
		deploymentID = cfg.DeploymentID
	}
	if deploymentID != "" {
		h.Set("x-grok-deployment-id", deploymentID)
	}
	if trace {
		h.Set("traceparent", traceparent())
		h.Set("tracestate", "")
	}
	h.Set("Authorization", "Bearer "+session.Token)
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "gzip")
	h.Set("User-Agent", defaultUserAgent(cfg))
	return h
}

// BuildHeaders is retained for source compatibility. New inference paths
// should use BuildInferenceHeaders with a request identity created once at the
// logical request boundary.
func BuildHeaders(cfg config.Config, session auth.Session, agentID, sessionID, convID, model string, trace bool) http.Header {
	return BuildInferenceHeaders(cfg, session, RequestIdentity{
		RequestID: NewID(), ConversationID: convID, SessionID: sessionID,
		AgentID: agentID, Model: model,
	}, trace)
}

func NewAgentID() string   { return NewID() }
func NewSessionID() string { return newUUID() }
