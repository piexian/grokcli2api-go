package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNoAuth = errors.New("no usable credentials in auths directory")

var (
	ErrInvalidCredentialJSON = errors.New("invalid credential JSON")
	ErrInvalidCredential     = errors.New("invalid credential")
)

type Session struct {
	Token         string   `json:"-"`
	Surface       string   `json:"surface"`
	UserID        string   `json:"user_id,omitempty"`
	AuthMode      AuthMode `json:"auth_mode,omitempty"`
	PrincipalType string   `json:"principal_type,omitempty"`
	PrincipalID   string   `json:"principal_id,omitempty"`
	OIDCIssuer    string   `json:"oidc_issuer,omitempty"`
	ObtainedAt    float64  `json:"obtained_at"`
	ExpiresAt     *float64 `json:"expires_at"`
}

func (s Session) IsAPIKey() bool { return s.AuthMode == AuthModeAPIKey }

func (s Session) Expired() bool {
	return s.ExpiresAt != nil && float64(time.Now().Unix()) >= *s.ExpiresAt-60
}

type RefreshError struct {
	Permanent bool
	Status    int
	Code      string
}

// AuthMode mirrors the provenance values persisted by Grok CLI. External and
// Web Login credentials are intentionally consumption-only in this proxy.
type AuthMode string

const (
	AuthModeOIDC     AuthMode = "oidc"
	AuthModeAPIKey   AuthMode = "api_key"
	AuthModeExternal AuthMode = "external"
	AuthModeWebLogin AuthMode = "web_login"
)

type subscriptionTier struct {
	Key     string
	Display string
	Paid    bool
	Billing bool
}

func (e *RefreshError) Error() string {
	if e.Code != "" {
		return "OAuth refresh failed: " + e.Code
	}
	return fmt.Sprintf("OAuth refresh failed with status %d", e.Status)
}

type credential struct {
	Path            string
	Raw             map[string]any
	Scope           string
	ScopeKey        string
	Scoped          bool
	TokensWrapper   bool
	AccessToken     string
	RefreshToken    string
	TokenURL        string
	ClientID        string
	Subject         string
	AuthMode        AuthMode
	PrincipalType   string
	PrincipalID     string
	OIDCIssuer      string
	Surface         string
	ExpiresAt       time.Time
	ExpiresIn       time.Duration
	Models          []string
	ModelsUpdatedAt time.Time
	Tier            subscriptionTier
	BotFlagged      bool
}

func loadCredential(path, surface string) (*credential, error) {
	credentials, err := loadCredentials(path, surface)
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("%w: no logical credentials", ErrInvalidCredential)
	}
	return credentials[0], nil
}

func loadCredentials(path, surface string) ([]*credential, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCredentials(b, path, surface)
}

func parseCredential(b []byte, path, surface string) (*credential, error) {
	credentials, err := parseCredentials(b, path, surface)
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("%w: no logical credentials", ErrInvalidCredential)
	}
	return credentials[0], nil
}

type credentialCandidate struct {
	scope         string
	node          map[string]any
	scoped        bool
	tokensWrapper bool
}

func parseCredentials(b []byte, path, surface string) ([]*credential, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredentialJSON, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: credential must be a JSON object", ErrInvalidCredential)
	}
	candidates := credentialCandidates(raw)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: credential has neither key nor refresh_token", ErrInvalidCredential)
	}
	result := make([]*credential, 0, len(candidates))
	for _, candidate := range candidates {
		cred, err := parseCredentialNode(raw, candidate, path, surface)
		if err != nil {
			return nil, err
		}
		result = append(result, cred)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: no usable logical credentials", ErrInvalidCredential)
	}
	return result, nil
}

func parseCredentialNode(raw map[string]any, candidate credentialCandidate, path, surface string) (*credential, error) {
	node := candidate.node
	access := firstString(node, "access_token", "AccessToken", "key", "session_token", "SessionToken")
	refresh := firstString(node, "refresh_token", "RefreshToken")
	if access == "" && refresh == "" {
		return nil, fmt.Errorf("%w: credential has neither access_token nor refresh_token", ErrInvalidCredential)
	}
	mode := parseAuthMode(firstString(node, "auth_mode", "authMode"))
	if mode == "" {
		switch {
		case normalizeScope(candidate.scope) == "xai::api_key":
			mode = AuthModeAPIKey
		case refresh != "" || firstString(node, "oidc_issuer", "issuer") != "":
			mode = AuthModeOIDC
		default:
			mode = AuthModeWebLogin
		}
	}
	accessClaims := jwtClaims(access)
	idClaims := jwtClaims(firstString(node, "id_token", "IDToken"))
	subject := firstString(node, "user_id", "userId", "UserId", "sub")
	if subject == "" {
		subject = claimString(accessClaims, "sub")
	}
	if subject == "" {
		subject = claimString(idClaims, "sub")
	}
	principalType := firstString(node, "principal_type", "principalType")
	principalID := firstString(node, "principal_id", "principalId", "team_id", "teamId")
	if principalID == "" {
		principalID = claimString(accessClaims, "principal_id")
	}
	if subject == "" {
		subject = principalID
	}
	if subject == "" && mode == AuthModeAPIKey {
		subject = "api-key:" + tokenIdentity(access)
	}
	if subject == "" {
		return nil, fmt.Errorf("%w: credential has no stable principal", ErrInvalidCredential)
	}
	clientID := firstString(node, "client_id", "clientId")
	if oidcClientID := firstString(node, "oidc_client_id", "oidcClientId"); oidcClientID != "" {
		clientID = oidcClientID
	}
	if clientID == "" {
		clientID = claimString(accessClaims, "client_id")
	}
	if clientID == "" {
		clientID = claimAudience(idClaims)
	}
	expiresIn := time.Duration(number(node["expires_in"])) * time.Second
	expiresAt := firstTime(node, "expired", "expires_at", "ExpiresAt", "expiry", "expiration")
	if expiresAt.IsZero() {
		expiresAt = unixClaimTime(accessClaims, "exp")
	}
	if expiresAt.IsZero() && expiresIn > 0 {
		if refreshed := firstTime(node, "last_refresh"); !refreshed.IsZero() {
			expiresAt = refreshed.Add(expiresIn)
		}
	}
	if expiresAt.IsZero() && mode != AuthModeAPIKey {
		if created := firstTime(node, "create_time", "createTime"); !created.IsZero() {
			expiresAt = created.Add(30 * 24 * time.Hour)
		}
	}
	return &credential{
		Path: path, Raw: cloneMap(raw), Scope: normalizeScope(candidate.scope), ScopeKey: candidate.scope, Scoped: candidate.scoped,
		TokensWrapper: candidate.tokensWrapper, AccessToken: access, RefreshToken: refresh,
		TokenURL: firstString(node, "token_endpoint", "tokenEndpoint"),
		ClientID: clientID, Subject: subject, AuthMode: mode,
		PrincipalType: principalType, PrincipalID: principalID,
		OIDCIssuer: strings.TrimRight(firstString(node, "oidc_issuer", "oidcIssuer", "issuer"), "/"),
		Surface:    defaultSurface(surface),
		ExpiresAt:  expiresAt, ExpiresIn: expiresIn,
		Models: stringSlice(node["models"]), ModelsUpdatedAt: firstTime(node, "models_updated_at"),
		Tier:       subscriptionTierFromClaims(accessClaims),
		BotFlagged: number(accessClaims["bot_flag_source"]) == 1 || number(idClaims["bot_flag_source"]) == 1,
	}, nil
}

func credentialNode(raw map[string]any) map[string]any {
	candidates := credentialCandidates(raw)
	if len(candidates) > 0 {
		return candidates[0].node
	}
	return raw
}

func credentialCandidates(raw map[string]any) []credentialCandidate {
	if isCredentialNode(raw) {
		return []credentialCandidate{{node: raw}}
	}
	if tokens, ok := raw["tokens"].(map[string]any); ok {
		if isCredentialNode(tokens) {
			return []credentialCandidate{{node: tokens, tokensWrapper: true}}
		}
		if candidates := scopedCandidates(tokens, true); len(candidates) > 0 {
			return candidates
		}
	}
	return scopedCandidates(raw, false)
}

func scopedCandidates(container map[string]any, wrapper bool) []credentialCandidate {
	keys := make([]string, 0, len(container))
	for scope, value := range container {
		if node, ok := value.(map[string]any); ok && isCredentialNode(node) {
			keys = append(keys, scope)
		}
	}
	sort.Strings(keys)
	result := make([]credentialCandidate, 0, len(keys))
	for _, scope := range keys {
		result = append(result, credentialCandidate{
			scope: scope, node: container[scope].(map[string]any), scoped: true, tokensWrapper: wrapper,
		})
	}
	return result
}

func isCredentialNode(node map[string]any) bool {
	return firstString(node, "access_token", "AccessToken", "refresh_token", "RefreshToken", "key", "session_token", "SessionToken") != ""
}

func parseAuthMode(value string) AuthMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "oidc":
		return AuthModeOIDC
	case "api_key", "apikey", "api-key":
		return AuthModeAPIKey
	case "external":
		return AuthModeExternal
	case "web_login", "weblogin", "grok":
		return AuthModeWebLogin
	default:
		return ""
	}
}

func normalizeScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return ""
	}
	if parsed, err := url.Parse(scope); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		parsed.RawQuery = ""
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		return parsed.String()
	}
	return strings.ToLower(strings.TrimRight(scope, "/"))
}

func tokenIdentity(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:12])
}

func (c *credential) session() Session {
	var expires *float64
	if !c.ExpiresAt.IsZero() {
		v := float64(c.ExpiresAt.UnixNano()) / 1e9
		expires = &v
	}
	return Session{
		Token: c.AccessToken, Surface: c.Surface, UserID: c.Subject, AuthMode: c.AuthMode,
		PrincipalType: c.PrincipalType, PrincipalID: c.PrincipalID, OIDCIssuer: c.OIDCIssuer,
		ObtainedAt: float64(time.Now().UnixNano()) / 1e9, ExpiresAt: expires,
	}
}

func (c *credential) needsRefresh(now time.Time, jitter time.Duration) bool {
	if c.AuthMode == AuthModeAPIKey {
		return false
	}
	if (c.AuthMode == AuthModeExternal || c.AuthMode == AuthModeWebLogin) && c.ExpiresAt.IsZero() {
		return true
	}
	if c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt.IsZero() {
		return false
	}
	return !now.Add(2*time.Minute + jitter).Before(c.ExpiresAt)
}

func (c *credential) usable(now time.Time) bool {
	if c.AccessToken == "" {
		return false
	}
	if (c.AuthMode == AuthModeExternal || c.AuthMode == AuthModeWebLogin) && c.ExpiresAt.IsZero() {
		return false
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return now.Add(time.Minute).Before(c.ExpiresAt)
}

func (c *credential) refresh(ctx context.Context, client *http.Client) (*credential, error) {
	if c.AuthMode != AuthModeOIDC {
		return nil, &RefreshError{Code: "credential_not_refreshable"}
	}
	if c.RefreshToken == "" || c.ClientID == "" {
		return nil, &RefreshError{Code: "missing_refresh_metadata"}
	}
	lock, err := acquireAuthFileLock(ctx, c.Path)
	if err != nil {
		return nil, err
	}
	defer lock.Close()

	// Re-read while holding the cross-process lock. A sibling may already have
	// rotated the token family; adopt its usable token rather than spending the
	// newly written refresh token a second time.
	source := c
	if disk, diskErr := loadMatchingCredential(c.Path, c.Surface, c); diskErr == nil {
		if disk.usable(time.Now()) && (disk.AccessToken != c.AccessToken || disk.RefreshToken != c.RefreshToken || disk.ExpiresAt.After(c.ExpiresAt)) {
			return disk, nil
		}
		source = disk
	}
	if source.RefreshToken == "" || source.ClientID == "" {
		return nil, &RefreshError{Code: "missing_refresh_metadata"}
	}
	endpoint, err := resolveTokenEndpoint(ctx, client, source)
	if err != nil {
		return nil, err
	}
	payload, status, err := exchangeRefreshToken(ctx, client, endpoint, source)
	if err != nil {
		return nil, err
	}
	access := firstString(payload, "access_token")
	if access == "" {
		return nil, &RefreshError{Status: status, Code: "missing_access_token"}
	}

	next := *source
	next.Raw = cloneMap(source.Raw)
	node := credentialNodeFor(next.Raw, source)
	if node == nil {
		return nil, fmt.Errorf("%w: target scope disappeared during refresh", ErrInvalidCredential)
	}
	// CLI 0.2.102 persists the access token as key. Keep an existing legacy
	// access_token alias synchronized so old files remain readable by old builds.
	node["key"] = access
	if _, legacyAlias := node["access_token"]; legacyAlias || !source.Scoped {
		node["access_token"] = access
	}
	next.AccessToken = access
	next.Tier = subscriptionTierFromClaims(jwtClaims(access))
	if refresh := firstString(payload, "refresh_token"); refresh != "" {
		node["refresh_token"] = refresh
	}
	if idToken := firstString(payload, "id_token"); idToken != "" {
		node["id_token"] = idToken
	}
	expiresSeconds := number(payload["expires_in"])
	if expiresSeconds <= 0 {
		expiresSeconds = int64(source.ExpiresIn / time.Second)
	}
	if expiresSeconds <= 0 {
		expiresSeconds = 21600
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(expiresSeconds) * time.Second)
	node["expires_in"] = expiresSeconds
	node["expires_at"] = expiresAt.Format(time.RFC3339Nano)
	node["create_time"] = now.Format(time.RFC3339Nano)
	if _, legacyExpiry := node["expired"]; legacyExpiry || !source.Scoped {
		node["expired"] = expiresAt.Format(time.RFC3339Nano)
		node["last_refresh"] = now.Format(time.RFC3339Nano)
	}
	if err := writeCredentialAtomicMode(c.Path, next.Raw, 0o600); err != nil {
		return nil, err
	}
	return loadMatchingCredential(c.Path, c.Surface, source)
}

func loadMatchingCredential(path, surface string, target *credential) (*credential, error) {
	credentials, err := loadCredentials(path, surface)
	if err != nil {
		return nil, err
	}
	for _, candidate := range credentials {
		if target.Scoped {
			if candidate.Scoped && candidate.Scope == target.Scope && candidate.AuthMode == target.AuthMode && candidate.PrincipalID == target.PrincipalID {
				return candidate, nil
			}
			continue
		}
		if !candidate.Scoped && candidate.Subject == target.Subject {
			return candidate, nil
		}
	}
	return nil, ErrCredentialNotFound
}

func credentialNodeFor(raw map[string]any, target *credential) map[string]any {
	if !target.Scoped {
		if target.TokensWrapper {
			node, _ := raw["tokens"].(map[string]any)
			return node
		}
		return raw
	}
	container := raw
	if target.TokensWrapper {
		container, _ = raw["tokens"].(map[string]any)
	}
	if container == nil {
		return nil
	}
	if node, ok := container[target.ScopeKey].(map[string]any); ok {
		return node
	}
	for scope, value := range container {
		if normalizeScope(scope) == target.Scope {
			node, _ := value.(map[string]any)
			return node
		}
	}
	return nil
}

type discoveryEntry struct {
	endpoint string
	at       time.Time
}

var discoveryCache = struct {
	sync.RWMutex
	entries map[string]discoveryEntry
}{entries: make(map[string]discoveryEntry)}

func resolveTokenEndpoint(ctx context.Context, client *http.Client, c *credential) (string, error) {
	if c.TokenURL != "" {
		parsed, err := url.Parse(c.TokenURL)
		if err != nil || !allowedTokenEndpoint(parsed) {
			return "", &RefreshError{Code: "invalid_token_endpoint"}
		}
		return parsed.String(), nil
	}
	issuer := strings.TrimRight(c.OIDCIssuer, "/")
	if issuer == "" {
		issuer = "https://auth.x.ai"
	}
	discoveryCache.RLock()
	cached, ok := discoveryCache.entries[issuer]
	discoveryCache.RUnlock()
	if ok && time.Since(cached.at) < time.Hour {
		return cached.endpoint, nil
	}
	discoveryURL := issuer + "/.well-known/openid-configuration"
	parsed, err := url.Parse(discoveryURL)
	if err != nil || !allowedTokenEndpoint(parsed) {
		return "", &RefreshError{Code: "invalid_oidc_issuer"}
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
		if reqErr != nil {
			return "", reqErr
		}
		req.Header.Set("Accept", "application/json")
		resp, requestErr := client.Do(req)
		if requestErr == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var document map[string]any
				if json.Unmarshal(body, &document) == nil {
					endpoint := firstString(document, "token_endpoint")
					endpointURL, parseErr := url.Parse(endpoint)
					if endpoint != "" && parseErr == nil && allowedTokenEndpoint(endpointURL) {
						discoveryCache.Lock()
						discoveryCache.entries[issuer] = discoveryEntry{endpoint: endpoint, at: time.Now()}
						discoveryCache.Unlock()
						return endpoint, nil
					}
				}
			}
			lastErr = &RefreshError{Status: resp.StatusCode, Code: "oidc_discovery_failed"}
		} else {
			lastErr = requestErr
		}
		if attempt == 0 && !waitRetry(ctx, 100*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

func exchangeRefreshToken(ctx context.Context, client *http.Client, endpoint string, c *credential) (map[string]any, int, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {c.ClientID},
	}
	if c.PrincipalType != "" {
		form.Set("principal_type", c.PrincipalType)
	}
	if c.PrincipalID != "" {
		form.Set("principal_id", c.PrincipalID)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, requestErr := client.Do(req)
		if requestErr == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else {
				payload := map[string]any{}
				_ = json.Unmarshal(body, &payload)
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return payload, resp.StatusCode, nil
				}
				code := strings.ToLower(firstString(payload, "error", "code"))
				refreshErr := &RefreshError{
					Permanent: code == "invalid_grant" || code == "invalid_client",
					Status:    resp.StatusCode, Code: code,
				}
				if refreshErr.Permanent {
					return nil, resp.StatusCode, refreshErr
				}
				lastErr = refreshErr
			}
		} else {
			lastErr = requestErr
		}
		if attempt < 2 && !waitRetry(ctx, time.Duration(attempt+1)*100*time.Millisecond) {
			return nil, 0, ctx.Err()
		}
	}
	return nil, 0, lastErr
}

func waitRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func writeCredentialAtomic(path string, raw map[string]any) error {
	return writeCredentialAtomicMode(path, raw, 0o600)
}

func writeCredentialAtomicMode(path string, raw map[string]any, mode os.FileMode) error {
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".grok-auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func allowedTokenEndpoint(u *url.URL) bool {
	if u == nil || u.Hostname() == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func accountID(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:12])
}

func (c *credential) accountID() string {
	if !c.Scoped {
		return accountID(c.Subject)
	}
	principal := c.PrincipalID
	if principal == "" {
		principal = c.Subject
	}
	canonical := c.Scope + "\x00" + string(c.AuthMode) + "\x00" + strings.ToLower(c.PrincipalType) + "\x00" + principal
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:12])
}

func deterministicJitter(id string) time.Duration {
	sum := sha256.Sum256([]byte(id))
	seconds := int64(sum[0])<<8 | int64(sum[1])
	return time.Duration(seconds%300) * time.Second
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

// subscriptionTierFromClaims mirrors the official Grok CLI mapping for the
// prod_auth.SubscriptionTier numeric JWT claim. Unknown positive values remain
// visible and are treated as paid, matching the CLI's fail-open future-tier policy.
func subscriptionTierFromClaims(claims map[string]any) subscriptionTier {
	value, ok := claims["tier"]
	if !ok {
		return subscriptionTier{}
	}
	n, ok := integerValue(value)
	if !ok || n < 0 {
		return subscriptionTier{}
	}
	switch n {
	case 0:
		return subscriptionTier{Key: "free", Display: "Free"}
	case 1:
		return subscriptionTier{Key: "supergrok", Display: "SuperGrok", Paid: true, Billing: true}
	case 2:
		return subscriptionTier{Key: "x_basic", Display: "X Basic", Paid: true}
	case 3:
		return subscriptionTier{Key: "x_premium", Display: "X Premium", Paid: true, Billing: true}
	case 4:
		return subscriptionTier{Key: "x_premium_plus", Display: "X Premium+", Paid: true, Billing: true}
	case 5:
		return subscriptionTier{Key: "supergrok_heavy", Display: "SuperGrok Heavy", Paid: true, Billing: true}
	case 6:
		return subscriptionTier{Key: "supergrok_lite", Display: "SuperGrok Lite", Paid: true, Billing: true}
	default:
		label := strconv.FormatInt(n, 10)
		return subscriptionTier{Key: label, Display: label, Paid: n > 0, Billing: n > 0}
	}
}

func integerValue(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		n := int64(v)
		return n, v == float64(n)
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func unixClaimTime(claims map[string]any, key string) time.Time {
	if claims == nil {
		return time.Time{}
	}
	seconds := number(claims[key])
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func claimString(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	v, _ := claims[key].(string)
	return v
}

func claimAudience(claims map[string]any) string {
	if claims == nil {
		return ""
	}
	switch v := claims["aud"].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return s
		}
	}
	return ""
}

func firstTime(node map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		value, ok := node[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				return t
			}
			if seconds, err := strconv.ParseInt(v, 10, 64); err == nil && seconds > 0 {
				return time.Unix(seconds, 0)
			}
		case float64:
			if v > 0 {
				return time.Unix(int64(v), 0)
			}
		case json.Number:
			if seconds, err := v.Int64(); err == nil && seconds > 0 {
				return time.Unix(seconds, 0)
			}
		}
	}
	return time.Time{}
}

func firstString(node map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := node[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func number(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func stringSlice(value any) []string {
	seen := map[string]struct{}{}
	var out []string
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			if model, ok := item.(string); ok && strings.TrimSpace(model) != "" {
				model = strings.TrimSpace(model)
				if _, duplicate := seen[model]; !duplicate {
					seen[model] = struct{}{}
					out = append(out, model)
				}
			}
		}
	case []string:
		for _, model := range values {
			model = strings.TrimSpace(model)
			if model != "" {
				if _, duplicate := seen[model]; !duplicate {
					seen[model] = struct{}{}
					out = append(out, model)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func cloneMap(raw map[string]any) map[string]any {
	b, _ := json.Marshal(raw)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func defaultSurface(v string) string {
	if v == "" {
		return "headless"
	}
	return v
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
