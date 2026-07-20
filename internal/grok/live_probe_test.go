package grok_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
	"github.com/Futureppo/grokcli2api-go/internal/server"
)

const (
	liveSmokeMaxResponseBytes = 32 << 20
	liveSmokeUnknownEffort    = "grok-live-smoke-unknown"
)

// TestLiveInferenceSmoke is deliberately guarded twice because it sends real
// generation requests. It never gives the service the source auth.json: only
// a 0600 temporary copy with refresh tokens, ID tokens, and email fields
// recursively removed is used. Response bodies and credential identifiers are
// intentionally never logged.
func TestLiveInferenceSmoke(t *testing.T) {
	if os.Getenv("GROK_LIVE_SMOKE") != "1" {
		t.Skip("set GROK_LIVE_SMOKE=1 to opt in to the live inference smoke")
	}
	if os.Getenv("GROK_LIVE_SMOKE_OFFLINE_GATES") != "passed" {
		t.Skip("set GROK_LIVE_SMOKE_OFFLINE_GATES=passed only after every offline gate succeeds")
	}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previousLogger)

	repository, err := liveSmokeRepository()
	if err != nil {
		t.Fatal("live smoke requires a Git worktree")
	}
	gitBefore, err := liveSmokeGitStatus(repository)
	if err != nil {
		t.Fatal("live smoke could not record the initial Git worktree state")
	}
	source := strings.TrimSpace(os.Getenv("GROK_LIVE_SMOKE_AUTH_FILE"))
	if source == "" {
		source = filepath.Join(repository, "auths", "live-01.json")
	} else if !filepath.IsAbs(source) {
		source = filepath.Join(repository, source)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		t.Fatal("live smoke credential path is invalid")
	}
	sourceBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatal("live smoke source credential could not be read")
	}
	sourceHash := sha256.Sum256(sourceBytes)
	defer func() {
		after, readErr := os.ReadFile(source)
		gitAfter, gitErr := liveSmokeGitStatus(repository)
		changed := readErr != nil || sha256.Sum256(after) != sourceHash || gitErr != nil || !bytes.Equal(gitBefore, gitAfter)
		if changed {
			t.Error("live smoke safety check failed: source credential or Git worktree changed")
		}
	}()

	sanitized, err := sanitizeLiveSmokeCredential(sourceBytes)
	if err != nil {
		t.Fatal("live smoke source credential is not a supported JSON object")
	}
	temporaryDirectory := t.TempDir()
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		t.Fatal("live smoke could not secure its temporary directory")
	}
	temporaryCredential := filepath.Join(temporaryDirectory, "auth.json")
	if err := os.WriteFile(temporaryCredential, sanitized, 0o600); err != nil {
		t.Fatal("live smoke could not create its isolated credential copy")
	}
	if err := os.Chmod(temporaryCredential, 0o600); err != nil {
		t.Fatal("live smoke could not secure its isolated credential copy")
	}
	if err := validateLiveSmokeCredentials(temporaryDirectory); err != nil {
		t.Fatal("live smoke credential validation failed; every non-API-key access token must remain valid for more than ten minutes")
	}

	cfg := liveSmokeConfig(temporaryDirectory)
	application, err := server.New(cfg)
	if err != nil {
		t.Fatal("live smoke service initialization failed")
	}
	defer application.Close()
	localServer := httptest.NewServer(application.Handler())
	defer localServer.Close()
	client := localServer.Client()
	client.Timeout = 2 * time.Minute

	requestContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	models, err := fetchLiveSmokeModels(requestContext, client, localServer.URL)
	cancel()
	if err != nil {
		t.Fatal("live smoke /v1/models request failed")
	}
	cases, err := liveSmokeGenerationCases(models)
	if err != nil {
		t.Fatal("live model catalog cannot cover the supported, none, and unknown effort probes within the six-call limit")
	}
	if len(cases) > 6 {
		t.Fatal("live smoke internal generation limit exceeded")
	}
	for _, probe := range cases {
		requestContext, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
		status, probeErr := runLiveSmokeGeneration(requestContext, client, localServer.URL, probe)
		cancel()
		if probeErr != nil {
			t.Fatalf("live generation probe failed (backend=%s stream=%t status=%d)", probe.backend, probe.stream, status)
		}
	}
	t.Logf("live inference smoke passed: backends=%d generation_calls=%d", len(models), len(cases))
}

func liveSmokeConfig(authsDirectory string) config.Config {
	chatBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("GROK_CHAT_PROXY_BASE_URL")), "/")
	if chatBaseURL == "" {
		chatBaseURL = "https://cli-chat-proxy.grok.com"
	}
	chatVersion := strings.Trim(strings.TrimSpace(os.Getenv("GROK_CHAT_PROXY_VERSION")), "/")
	if chatVersion == "" {
		chatVersion = "v1"
	}
	xaiBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("GROK_XAI_API_BASE_URL")), "/")
	if xaiBaseURL == "" {
		xaiBaseURL = "https://api.x.ai"
	}
	return config.Config{
		ChatProxyBaseURL: chatBaseURL, ChatProxyVersion: chatVersion, XAIAPIBaseURL: xaiBaseURL,
		AuthsDir: authsDirectory, AuthsReloadInterval: 24 * time.Hour,
		AuthRefreshConcurrency: 1, AccountMaxInflight: 1, ModelsRefreshInterval: 24 * time.Hour,
		RetryMaxAttempts: 1, RetryBaseDelay: 200 * time.Millisecond,
		RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 128,
		ClientName: "grok-shell", ClientVersion: "0.2.102", ClientSurface: "tui", ClientMode: "headless",
		ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", DeploymentID: strings.TrimSpace(os.Getenv("GROK_DEPLOYMENT_ID")),
		StreamCompression: "identity", ProxyURL: strings.TrimSpace(os.Getenv("GROK_PROXY_URL")),
	}
}

type rejectLiveSmokeNetwork struct{}

func (rejectLiveSmokeNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled during isolated credential validation")
}

func validateLiveSmokeCredentials(directory string) error {
	client := &http.Client{Transport: rejectLiveSmokeNetwork{}}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: directory, Surface: "tui", ReloadInterval: time.Hour,
		RefreshConcurrency: 1, AccountMaxInflight: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 16,
	}, client)
	if err != nil {
		return err
	}
	defer pool.Close()
	credentials := pool.Credentials()
	if len(credentials) != 1 {
		return errors.New("live smoke requires exactly one logical credential")
	}
	minimumExpiry := time.Now().Add(10 * time.Minute)
	for _, credential := range credentials {
		if credential.HasRefreshToken {
			return errors.New("temporary credential retained a refresh token")
		}
		if credential.AuthMode == auth.AuthModeAPIKey {
			continue
		}
		if credential.ExpiresAt == nil || !credential.ExpiresAt.After(minimumExpiry) || !credential.Usable {
			return errors.New("access token expires too soon")
		}
	}
	return nil
}

func sanitizeLiveSmokeCredential(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("credential contains trailing JSON")
	}
	root, ok := value.(map[string]any)
	if !ok || root == nil {
		return nil, errors.New("credential must be an object")
	}
	scrubLiveSmokeSecrets(root)
	if liveSmokeSecretsRemain(root) {
		return nil, errors.New("credential secret fields remain")
	}
	return json.MarshalIndent(root, "", "  ")
}

func scrubLiveSmokeSecrets(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if isLiveSmokeSecretField(key) || isLiveSmokeTransientCatalogField(key) {
				delete(value, key)
				continue
			}
			scrubLiveSmokeSecrets(child)
		}
	case []any:
		for _, child := range value {
			scrubLiveSmokeSecrets(child)
		}
	}
}

func liveSmokeSecretsRemain(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if isLiveSmokeSecretField(key) || isLiveSmokeTransientCatalogField(key) || liveSmokeSecretsRemain(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if liveSmokeSecretsRemain(child) {
				return true
			}
		}
	}
	return false
}

func isLiveSmokeSecretField(key string) bool {
	name := normalizedLiveSmokeField(key)
	return strings.Contains(name, "refreshtoken") || strings.Contains(name, "idtoken") || strings.Contains(name, "email")
}

func isLiveSmokeTransientCatalogField(key string) bool {
	switch normalizedLiveSmokeField(key) {
	case "models", "modelsupdatedat":
		return true
	default:
		return false
	}
}

func normalizedLiveSmokeField(key string) string {
	var normalized strings.Builder
	for _, character := range strings.ToLower(key) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			normalized.WriteRune(character)
		}
	}
	return normalized.String()
}

type liveSmokeModel struct {
	id              string
	supportedEffort string
}

type liveSmokeModelsResponse struct {
	Data []struct {
		ID    string `json:"id"`
		XGrok struct {
			APIBackends             []string `json:"api_backends"`
			ReasoningEfforts        []string `json:"reasoning_efforts"`
			SupportsReasoningEffort bool     `json:"supports_reasoning_effort"`
		} `json:"x_grok"`
	} `json:"data"`
}

func fetchLiveSmokeModels(ctx context.Context, client *http.Client, baseURL string) (map[string]liveSmokeModel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, liveSmokeMaxResponseBytes+1))
	if err != nil || len(payload) > liveSmokeMaxResponseBytes {
		return nil, errors.New("model response is unreadable")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model status %d", response.StatusCode)
	}
	var catalog liveSmokeModelsResponse
	if json.Unmarshal(payload, &catalog) != nil {
		return nil, errors.New("model response is invalid")
	}
	models := make(map[string]liveSmokeModel)
	for _, item := range catalog.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		effort := liveSmokeSupportedEffort(item.XGrok.ReasoningEfforts, item.XGrok.SupportsReasoningEffort)
		for _, backend := range item.XGrok.APIBackends {
			backend = strings.ToLower(strings.TrimSpace(backend))
			if backend != "chat_completions" && backend != "responses" && backend != "messages" {
				continue
			}
			current, exists := models[backend]
			if !exists || current.supportedEffort == "" && effort != "" {
				models[backend] = liveSmokeModel{id: item.ID, supportedEffort: effort}
			}
		}
	}
	if len(models) == 0 {
		return nil, errors.New("no structured backend descriptors")
	}
	return models, nil
}

func liveSmokeSupportedEffort(efforts []string, supports bool) string {
	if !supports {
		return ""
	}
	if len(efforts) == 0 {
		return "low"
	}
	for _, preferred := range []string{"low", "minimal", "medium", "high", "xhigh"} {
		for _, effort := range efforts {
			if strings.EqualFold(strings.TrimSpace(effort), preferred) {
				return preferred
			}
		}
	}
	return ""
}

type liveSmokeGeneration struct {
	backend string
	model   liveSmokeModel
	stream  bool
	effort  string
}

func liveSmokeGenerationCases(models map[string]liveSmokeModel) ([]liveSmokeGeneration, error) {
	var cases []liveSmokeGeneration
	for _, backend := range []string{"chat_completions", "responses", "messages"} {
		model, exists := models[backend]
		if !exists {
			continue
		}
		fallback := model.supportedEffort
		if fallback == "" {
			fallback = "low"
		}
		cases = append(cases,
			liveSmokeGeneration{backend: backend, model: model, effort: fallback},
			liveSmokeGeneration{backend: backend, model: model, stream: true, effort: fallback},
		)
	}
	if len(cases) < 3 || len(cases) > 6 {
		return nil, errors.New("insufficient backend probe slots")
	}
	supported := -1
	for index := range cases {
		if cases[index].model.supportedEffort != "" {
			supported = index
			cases[index].effort = cases[index].model.supportedEffort
			break
		}
	}
	if supported < 0 {
		return nil, errors.New("no model advertises a supported effort")
	}
	assigned := 0
	for index := range cases {
		if index == supported {
			continue
		}
		if assigned == 0 {
			cases[index].effort = "none"
		} else if assigned == 1 {
			cases[index].effort = liveSmokeUnknownEffort
			break
		}
		assigned++
	}
	if assigned < 1 {
		return nil, errors.New("effort probes could not be assigned")
	}
	return cases, nil
}

func runLiveSmokeGeneration(ctx context.Context, client *http.Client, baseURL string, probe liveSmokeGeneration) (int, error) {
	path, body := liveSmokeGenerationRequest(probe)
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	if probe.backend == "messages" {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	responsePayload, err := io.ReadAll(io.LimitReader(response.Body, liveSmokeMaxResponseBytes+1))
	if err != nil || len(responsePayload) > liveSmokeMaxResponseBytes {
		return response.StatusCode, errors.New("generation response is unreadable")
	}
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, errors.New("generation status is not successful")
	}
	if probe.stream {
		return response.StatusCode, validateLiveSmokeStream(probe.backend, responsePayload)
	}
	return response.StatusCode, validateLiveSmokeJSON(responsePayload)
}

func liveSmokeGenerationRequest(probe liveSmokeGeneration) (string, map[string]any) {
	common := map[string]any{
		"model": probe.model.id, "stream": probe.stream, "store": false,
	}
	switch probe.backend {
	case "chat_completions":
		common["messages"] = []any{map[string]any{"role": "user", "content": "Reply with OK."}}
		common["max_tokens"] = 16
		common["reasoning_effort"] = probe.effort
		return "/v1/chat/completions", common
	case "messages":
		common["messages"] = []any{map[string]any{"role": "user", "content": "Reply with OK."}}
		common["max_tokens"] = 16
		common["output_config"] = map[string]any{"effort": probe.effort}
		return "/v1/messages", common
	default:
		common["input"] = "Reply with OK."
		common["max_output_tokens"] = 16
		common["reasoning"] = map[string]any{"effort": probe.effort}
		return "/v1/responses", common
	}
}

func validateLiveSmokeJSON(payload []byte) error {
	var response map[string]any
	if len(payload) == 0 || json.Unmarshal(payload, &response) != nil {
		return errors.New("generation response is not JSON")
	}
	if response["error"] != nil || strings.EqualFold(fmt.Sprint(response["type"]), "error") || strings.EqualFold(fmt.Sprint(response["status"]), "failed") {
		return errors.New("generation returned an application error")
	}
	return nil
}

func validateLiveSmokeStream(backend string, payload []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 64<<10), liveSmokeMaxResponseBytes)
	eventName := ""
	dataLines := make([]string, 0, 1)
	success, failed := false, false
	firstLine := true
	flush := func() {
		data := strings.Join(dataLines, "\n")
		name := strings.ToLower(strings.TrimSpace(eventName))
		if data == "[DONE]" {
			success = backend == "chat_completions"
		}
		var envelope struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		}
		if data != "" {
			_ = json.Unmarshal([]byte(data), &envelope)
		}
		if name == "" {
			name = strings.ToLower(strings.TrimSpace(envelope.Type))
		}
		switch backend {
		case "responses":
			switch name {
			case "response.completed", "response.incomplete":
				success = true
			case "response.failed", "error":
				failed = true
			}
			if strings.EqualFold(envelope.Status, "failed") {
				failed = true
			}
		case "messages":
			if name == "message_stop" {
				success = true
			} else if name == "error" {
				failed = true
			}
		case "chat_completions":
			if name == "error" {
				failed = true
			}
		}
		eventName = ""
		dataLines = dataLines[:0]
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if line == "" {
			flush()
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if len(dataLines) > 0 || eventName != "" {
		flush()
	}
	if scanner.Err() != nil || failed || !success {
		return errors.New("generation stream did not end successfully")
	}
	return nil
}

func liveSmokeRepository() (string, error) {
	command := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("empty repository root")
	}
	return filepath.Clean(root), nil
}

func liveSmokeGitStatus(repository string) ([]byte, error) {
	command := exec.Command("git", "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	command.Dir = repository
	return command.Output()
}

func TestSanitizeLiveSmokeCredential(t *testing.T) {
	raw := []byte(`{
		"models": ["stale-model"],
		"models_updated_at": "2026-01-01T00:00:00Z",
		"tokens": {
			"scope:a": {
				"access_token": "keep",
				"refreshToken": "remove",
				"profile": {"email_address": "remove", "ID-Token": "remove"},
				"metadata": [{"emailVerified": true}, {"safe": "keep"}]
			}
		}
	}`)
	sanitized, err := sanitizeLiveSmokeCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sanitized, []byte("remove")) || bytes.Contains(sanitized, []byte("email")) || bytes.Contains(sanitized, []byte("ID-Token")) || bytes.Contains(sanitized, []byte("stale-model")) {
		t.Fatalf("sensitive fields remain in sanitized credential: %s", sanitized)
	}
	if !bytes.Contains(sanitized, []byte(`"access_token": "keep"`)) || !bytes.Contains(sanitized, []byte(`"safe": "keep"`)) {
		t.Fatalf("sanitizer removed required fields: %s", sanitized)
	}
}

func TestLiveSmokeGenerationCases(t *testing.T) {
	models := map[string]liveSmokeModel{
		"chat_completions": {id: "chat", supportedEffort: "medium"},
		"responses":        {id: "responses", supportedEffort: "high"},
		"messages":         {id: "messages", supportedEffort: "low"},
	}
	cases, err := liveSmokeGenerationCases(models)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 6 {
		t.Fatalf("generation cases=%d, want 6", len(cases))
	}
	efforts := map[string]bool{}
	counts := map[string]map[bool]int{}
	for _, probe := range cases {
		efforts[probe.effort] = true
		if counts[probe.backend] == nil {
			counts[probe.backend] = map[bool]int{}
		}
		counts[probe.backend][probe.stream]++
	}
	if !efforts["medium"] || !efforts["none"] || !efforts[liveSmokeUnknownEffort] {
		t.Fatalf("effort coverage=%v", efforts)
	}
	for backend, modes := range counts {
		if modes[false] != 1 || modes[true] != 1 {
			t.Fatalf("backend %s modes=%v", backend, modes)
		}
	}
}
