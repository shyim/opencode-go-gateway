package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	agentName            = "opencode-go-gateway/0.1"
	defaultClientVersion = "2.2.5"
	compatibilityEnv     = "<env>\n  Working directory: /workspace\n</env>"
)

var compatibilityReadTool = json.RawMessage(`{"type":"function","function":{"name":"read","description":"Read a file from the workspace.","parameters":{"type":"object","properties":{}}}}`)

func (m model) key() string { return m.ProviderID + "/" + m.ID }

// freeTierEnvelope mirrors the request shape the OpenCode CLI sends to the
// keyless public provider. Its free tier rejects otherwise valid requests that
// lack an environment system block and a function tool named "read". This is a
// heuristic based on observed upstream behavior and may need updating.
func freeTierEnvelope(provider string, payload map[string]json.RawMessage) {
	if provider != "opencode" {
		return
	}

	addCompatibilityEnv(payload)
	addCompatibilityReadTool(payload)
	addCompatibilityStream(payload)
}

func addCompatibilityEnv(payload map[string]json.RawMessage) {
	var messages []json.RawMessage
	if json.Unmarshal(payload["messages"], &messages) != nil || len(messages) == 0 {
		return
	}

	firstSystem := -1

	for i, message := range messages {
		fields := messageFields(message)
		if fields == nil || string(fields["role"]) != `"system"` {
			continue
		}

		if firstSystem < 0 {
			firstSystem = i
		}

		content := fields["content"]
		if hasCompatibilityEnv(content) {
			return
		}
	}

	if firstSystem >= 0 {
		fields := messageFields(messages[firstSystem])
		if content, ok := withCompatibilityEnv(fields["content"]); ok {
			fields["content"] = content
			if updated, err := json.Marshal(fields); err == nil {
				messages[firstSystem] = updated
				setPayloadMessages(payload, messages)
			}

			return
		}
	}

	message, _ := json.Marshal(map[string]string{"role": "system", "content": compatibilityEnv})
	setPayloadMessages(payload, append([]json.RawMessage{message}, messages...))
}

func hasCompatibilityEnv(content json.RawMessage) bool {
	var text strings.Builder

	if len(content) > 0 && content[0] == '"' {
		var value string
		if json.Unmarshal(content, &value) == nil {
			text.WriteString(value)
		}
	} else if len(content) > 0 && content[0] == '[' {
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(content, &parts) == nil {
			for _, part := range parts {
				text.WriteString(part.Text)
				text.WriteString("\n")
			}
		}
	}

	value := text.String()

	return strings.Contains(value, "<env>") && strings.Contains(value, "Working directory:")
}

func withCompatibilityEnv(content json.RawMessage) (json.RawMessage, bool) {
	if len(content) > 0 && content[0] == '"' {
		var text string
		if json.Unmarshal(content, &text) != nil {
			return nil, false
		}

		merged, err := json.Marshal(text + "\n\n" + compatibilityEnv)

		return merged, err == nil
	}

	if len(content) > 0 && content[0] == '[' {
		var parts []json.RawMessage
		if json.Unmarshal(content, &parts) != nil {
			return nil, false
		}

		part, _ := json.Marshal(map[string]string{"type": "text", "text": compatibilityEnv})
		merged, err := json.Marshal(append(parts, part))

		return merged, err == nil
	}

	return nil, false
}

func addCompatibilityReadTool(payload map[string]json.RawMessage) {
	var tools []json.RawMessage
	if raw, ok := payload["tools"]; ok && string(raw) != jsonNull {
		if json.Unmarshal(raw, &tools) != nil {
			return
		}
	}

	if len(tools) == 0 {
		payload["tools"], _ = json.Marshal([]json.RawMessage{compatibilityReadTool})
		payload["tool_choice"], _ = json.Marshal("none")

		return
	}

	for _, tool := range tools {
		var definition struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(tool, &definition) == nil && definition.Function.Name == "read" {
			return
		}
	}
	// Adding a tool the caller did not ask for could produce calls the caller
	// cannot execute, so only do so when tools are disabled explicitly.
	if string(payload["tool_choice"]) != `"none"` {
		return
	}

	payload["tools"], _ = json.Marshal(append(tools, compatibilityReadTool))
}

// addCompatibilityStream forces streaming because the keyless free tier rejects
// non-streaming completions. The gateway aggregates the stream again for callers
// that did not request streaming.
func addCompatibilityStream(payload map[string]json.RawMessage) {
	var stream bool
	if json.Unmarshal(payload["stream"], &stream) == nil && stream {
		return
	}

	payload["stream"], _ = json.Marshal(true)
	if _, ok := payload["stream_options"]; !ok {
		payload["stream_options"], _ = json.Marshal(map[string]bool{"include_usage": true})
	}
}

func messageFields(message json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(message, &fields) != nil || fields == nil {
		return nil
	}

	return fields
}

func setPayloadMessages(payload map[string]json.RawMessage, messages []json.RawMessage) {
	if body, err := json.Marshal(messages); err == nil {
		payload["messages"] = body
	}
}

func zeroCache(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == jsonNull {
		return true
	}

	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}

	return zeroCacheValue(value)
}

func zeroCacheValue(value any) bool {
	switch value := value.(type) {
	case float64:
		return value == 0
	case map[string]any:
		for _, nested := range value {
			if !zeroCacheValue(nested) {
				return false
			}
		}

		return true
	default:
		return false
	}
}

func (p *catalogPrice) free() bool {
	if p == nil || p.Price == nil || *p.Price != 0 || !zeroCache(p.Cache) {
		return false
	}

	for _, o := range p.Overrides {
		if (o.Price != nil && *o.Price != 0) || !zeroCache(o.Cache) {
			return false
		}
	}

	return true
}

func freeCost(raw json.RawMessage) bool {
	var c catalogCost
	if json.Unmarshal(raw, &c) != nil {
		return false
	}

	var all any
	if json.Unmarshal(raw, &all) != nil {
		return false
	}

	return c.Text.Input.free() && c.Text.Output.free() && pricesZero(all)
}

func pricesZero(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if key == "price" {
				price, ok := item.(float64)
				if !ok || price != 0 {
					return false
				}
			}

			if key == "cache" && item != nil && !zeroCacheValue(item) {
				return false
			}

			if !pricesZero(item) {
				return false
			}
		}
	case []any:
		for _, item := range value {
			if !pricesZero(item) {
				return false
			}
		}
	}

	return true
}

func newUpstream(catalog, console, goBase *url.URL, key string) *upstream {
	if key == "" {
		key = "public"
	}

	return &upstream{
		catalogURL:    catalog,
		consoleURL:    console,
		goURL:         goBase,
		key:           key,
		clientVersion: defaultClientVersion,
		clientName:    "cli",
		clientChannel: "latest",
		http: &http.Client{
			Transport: newCLITransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (o *upstream) catalog(ctx context.Context) ([]model, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if time.Now().Before(o.expires) {
		return append([]model(nil), o.cached...), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.catalogURL.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", agentName)

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch model catalog: HTTP %d", resp.StatusCode)
	}

	var data map[string]catalogProvider
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}

	if _, ok := data["opencode"]; !ok {
		return nil, errors.New("model catalog has no OpenCode provider")
	}

	var result []model

	for _, provider := range []string{"opencode", "opencode-go"} {
		// OpenCode enables the Go provider only after a connection is configured.
		if provider == "opencode-go" && o.key == "public" {
			continue
		}

		for key, m := range data[provider].Models {
			if !m.available(key) {
				continue
			}

			result = append(result, model{ID: m.ID, ProviderID: provider, Name: m.Name})
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i].key() < result[j].key() })
	o.cached = result
	o.expires = time.Now().Add(5 * time.Minute)

	return append([]model(nil), result...), nil
}

func (m catalogModel) available(key string) bool {
	_, chatAPI := m.API["chat-completions"]

	return m.ID != "" && key == m.ID && !strings.ContainsAny(m.ID, "/?#") &&
		m.Type == "chat" && m.Status != "deprecated" && chatAPI &&
		slices.Contains(m.Modalities.Input, "text") && slices.Contains(m.Modalities.Output, "text") && freeCost(m.Cost)
}

func resolveModel(models []model, name string) (model, error) {
	var found []model

	for _, m := range models {
		if m.key() == name || m.ID == name {
			found = append(found, m)
		}
	}

	if len(found) == 1 {
		return found[0], nil
	}

	if len(found) > 1 {
		return model{}, fmt.Errorf("model %q is ambiguous; use provider/model", name)
	}

	return model{}, fmt.Errorf("free model %q is not available; see /v1/models", name)
}

func (o *upstream) completion(ctx context.Context, m model, body []byte, identity requestIdentity) (*http.Response, error) {
	base := o.consoleURL
	if m.ProviderID == "opencode-go" {
		base = o.goURL
	}

	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	u.RawPath = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "opencode/"+o.clientChannel+"/"+o.clientVersion+"/"+o.clientName)
	req.Header.Set("x-opencode-session", identity.affinity)
	req.Header.Set("x-opencode-session-id", identity.session)
	req.Header.Set("x-session-affinity", identity.affinity)
	req.Header.Set("x-session-id", identity.affinity)

	if identity.parent != "" {
		req.Header.Set("x-opencode-parent-session-id", identity.parent)
		req.Header.Set("x-parent-session-id", identity.parent)
	}

	req.Header.Set("x-opencode-client", o.clientName)
	req.Header.Set("x-opencode-project", identity.project)

	return o.http.Do(req)
}
