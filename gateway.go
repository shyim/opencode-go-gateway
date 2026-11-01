package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (g *gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodError(w)
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/models", g.auth(g.models))
	mux.HandleFunc("/v1/chat/completions", g.auth(g.completions))

	return mux
}

func (g *gateway) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.apiKey != "" {
			header := r.Header.Get("Authorization")

			token := strings.TrimPrefix(header, "Bearer ")
			if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare([]byte(token), []byte(g.apiKey)) != 1 {
				apiError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid gateway API key")
				return
			}
		}

		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func apiError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": code, "code": code, "param": nil}})
}

func methodError(w http.ResponseWriter) {
	apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
}

func upstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}

	apiError(w, status, "upstream_error", err.Error())
}

func (g *gateway) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodError(w)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.timeout)
	defer cancel()

	models, err := g.upstream.catalog(ctx)
	if err != nil {
		upstreamError(w, err)
		return
	}

	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{"id": m.key(), "object": "model", "created": 0, "owned_by": m.ProviderID})
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func sessionID(r *http.Request) (string, error) {
	var id string
	for _, name := range []string{"X-Opencode-Session-Id", "X-Opencode-Session", "X-Session-Id", "X-Session-Affinity"} {
		if id = r.Header.Get(name); id != "" {
			break
		}
	}

	if id != "" {
		return id, nil
	}

	var b [14]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	// OpenCode's identifiers start with a descending, 48-bit millisecond timestamp.
	timestamp := ^((uint64(time.Now().UnixMilli()) << 12) + 1) & 0xffffffffffff

	return fmt.Sprintf("ses_%012x%s", timestamp, b[:]), nil
}

func isAllowedIDChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_'
}

func validateID(name, value string) error {
	if len(value) > 128 || strings.IndexFunc(value, func(c rune) bool { return !isAllowedIDChar(c) }) >= 0 {
		return fmt.Errorf("%s must contain at most 128 letters, digits, underscores, or hyphens", name)
	}

	return nil
}

func firstHeader(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := r.Header.Get(name); value != "" {
			return value
		}
	}

	return ""
}

func identity(r *http.Request) (requestIdentity, error) {
	session, err := sessionID(r)
	if err != nil {
		return requestIdentity{}, err
	}

	parent := firstHeader(r, "X-Opencode-Parent-Session-Id", "X-Parent-Session-Id")

	affinity := firstHeader(r, "X-Opencode-Session", "X-Session-Affinity", "X-Session-Id")
	if affinity == "" {
		affinity = parent
	}

	if affinity == "" {
		affinity = session
	}

	project := firstHeader(r, "X-Opencode-Project")
	if project == "" {
		project = "global"
	}

	for name, value := range map[string]string{
		"session":  session,
		"affinity": affinity,
		"parent":   parent,
		"project":  project,
	} {
		if err := validateID(name, value); err != nil {
			return requestIdentity{}, err
		}
	}

	return requestIdentity{session: session, affinity: affinity, parent: parent, project: project}, nil
}

func parseCompletionPayload(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, string, bool, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		return nil, "", false, fmt.Errorf("body_too_large: %w", err)
	}

	var payload map[string]json.RawMessage
	if err = json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, "", false, errors.New("request must be a JSON object")
	}

	var name string
	if json.Unmarshal(payload["model"], &name) != nil || name == "" {
		return nil, "", false, errors.New("model is required")
	}

	var messages []json.RawMessage
	if json.Unmarshal(payload["messages"], &messages) != nil || len(messages) == 0 {
		return nil, "", false, errors.New("messages must be a nonempty array")
	}

	var clientStream bool

	_ = json.Unmarshal(payload["stream"], &clientStream)

	if tier, ok := payload["service_tier"]; ok && string(tier) != jsonNull && string(tier) != `"default"` && string(tier) != `"auto"` {
		return nil, "", false, errors.New("non-default service tiers are not supported")
	}

	return payload, name, clientStream, nil
}

func prepareCompletionPayload(m model, payload map[string]json.RawMessage) ([]byte, error) {
	modelBytes, err := json.Marshal(m.ID)
	if err != nil {
		return nil, err
	}

	payload["model"] = modelBytes
	freeTierEnvelope(m.ProviderID, payload)

	return json.Marshal(payload)
}

func (g *gateway) completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodError(w)
		return
	}
	defer func() { _ = r.Body.Close() }()

	payload, name, clientStream, err := parseCompletionPayload(w, r)
	if err != nil {
		if strings.HasPrefix(err.Error(), "body_too_large") {
			apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body is too large")
			return
		}

		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())

		return
	}

	reqIdentity, err := identity(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.timeout)
	defer cancel()

	models, err := g.upstream.catalog(ctx)
	if err != nil {
		upstreamError(w, err)
		return
	}

	m, err := resolveModel(models, name)
	if err != nil {
		apiError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	body, err := prepareCompletionPayload(m, payload)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	resp, err := g.upstream.completion(ctx, m, body, reqIdentity)
	if err != nil {
		upstreamError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	g.relayCompletionResponse(w, resp, m, reqIdentity, clientStream)
}

func (g *gateway) relayCompletionResponse(w http.ResponseWriter, resp *http.Response, m model, reqIdentity requestIdentity, clientStream bool) {
	w.Header().Set("X-Opencode-Session-Id", reqIdentity.session)
	w.Header().Set("X-Opencode-Session", reqIdentity.affinity)

	for _, h := range []string{"Retry-After", "X-Request-Id"} {
		if value := resp.Header.Get(h); value != "" {
			w.Header().Set(h, value)
		}
	}

	isEventStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	isSuccess := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices

	if m.ProviderID == "opencode" && !clientStream && isSuccess && isEventStream {
		body, err := aggregateStream(resp.Body)
		if err != nil {
			upstreamError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)

		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	if isEventStream {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}

	w.WriteHeader(resp.StatusCode)

	if flusher, ok := w.(http.Flusher); ok && isEventStream {
		_, _ = io.Copy(flushingWriter{w, flusher}, resp.Body)
		return
	}

	_, _ = io.Copy(w, resp.Body)
}

func (w flushingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.flusher.Flush()

	return n, err
}
