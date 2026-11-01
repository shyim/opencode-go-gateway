package main

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"
)

func TestFreeTierEnvelopePlainRequest(t *testing.T) {
	t.Parallel()

	payload := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"hi"}]`),
	}
	freeTierEnvelope("opencode", payload)

	messages := decodePayload[[]map[string]any](t, payload["messages"])
	if len(messages) != 2 || messages[0]["role"] != "system" || messages[1]["content"] != "hi" {
		t.Fatalf("messages %#v", messages)
	}

	content, _ := messages[0]["content"].(string)
	if !strings.Contains(content, "<env>") || !strings.Contains(content, "Working directory: /workspace") {
		t.Fatalf("system content %q", content)
	}

	tools := decodePayload[[]map[string]any](t, payload["tools"])
	if len(tools) != 1 || asType[map[string]any](t, tools[0]["function"])["name"] != "read" {
		t.Fatalf("tools %#v", tools)
	}

	if choice := decodePayload[string](t, payload["tool_choice"]); choice != "none" {
		t.Fatalf("tool_choice %q", choice)
	}
}

func TestFreeTierEnvelopeMergesIntoSystemMessage(t *testing.T) {
	t.Parallel()

	payload := map[string]json.RawMessage{
		"messages":    json.RawMessage(`[{"role":"system","content":[{"type":"text","text":"be brief"}]},{"role":"user","content":"hi"}]`),
		"tools":       json.RawMessage(`[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]`),
		"tool_choice": json.RawMessage(`"auto"`),
	}
	freeTierEnvelope("opencode", payload)

	messages := decodePayload[[]map[string]any](t, payload["messages"])
	if len(messages) != 2 {
		t.Fatalf("messages %#v", messages)
	}

	parts, _ := messages[0]["content"].([]any)
	if len(parts) != 2 || !strings.Contains(asType[string](t, asType[map[string]any](t, parts[1])["text"]), "<env>") {
		t.Fatalf("system content %#v", messages[0]["content"])
	}

	if tools := decodePayload[[]map[string]any](t, payload["tools"]); len(tools) != 1 {
		t.Fatalf("tools %#v", tools)
	}

	if choice := decodePayload[string](t, payload["tool_choice"]); choice != "auto" {
		t.Fatalf("tool_choice %q", choice)
	}
}

func TestFreeTierEnvelopeLeavesCallerToolsAlone(t *testing.T) {
	t.Parallel()

	body := `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`

	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}

	freeTierEnvelope("opencode", payload)

	tools := decodePayload[[]map[string]any](t, payload["tools"])
	if len(tools) != 1 || asType[map[string]any](t, tools[0]["function"])["name"] != "lookup" {
		t.Fatalf("tools %#v", tools)
	}

	if _, ok := payload["tool_choice"]; ok {
		t.Fatalf("tool_choice was added: %s", payload["tool_choice"])
	}

	messages := decodePayload[[]map[string]any](t, payload["messages"])
	if content, _ := messages[0]["content"].(string); !strings.HasSuffix(content, compatibilityEnv) {
		t.Fatalf("system content %q", content)
	}
}

func TestFreeTierEnvelopeIsIdempotent(t *testing.T) {
	t.Parallel()

	payload := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"hi"}]`),
	}
	freeTierEnvelope("opencode", payload)
	first := string(payload["messages"])
	freeTierEnvelope("opencode", payload)

	if string(payload["messages"]) != first {
		t.Fatalf("messages changed: %s -> %s", first, payload["messages"])
	}

	if tools := decodePayload[[]map[string]any](t, payload["tools"]); len(tools) != 1 {
		t.Fatalf("tools %#v", tools)
	}
}

func TestFreeTierEnvelopeSkipsGoProvider(t *testing.T) {
	t.Parallel()

	payload := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"hi"}]`),
	}

	before := map[string]json.RawMessage{}
	maps.Copy(before, payload)

	freeTierEnvelope("opencode-go", payload)

	if !reflect.DeepEqual(payload, before) {
		t.Fatalf("go payload changed: %#v", payload)
	}
}

func decodePayload[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()

	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}

	return value
}

func TestFreeCost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cost string
		free bool
	}{
		{"zero prices", `{"text":{"input":{"price":0,"cache":{"read":0,"write":0}},"output":{"price":0}}}`, true},
		{"missing input price", `{"text":{"input":{},"output":{"price":0}}}`, false},
		{"paid output", `{"text":{"input":{"price":0},"output":{"price":1}}}`, false},
		{"paid override", `{"text":{"input":{"price":0,"overrides":[{"price":1}]},"output":{"price":0}}}`, false},
		{"paid nested cache", `{"text":{"input":{"price":0,"cache":{"explicit":{"write":1}}},"output":{"price":0}}}`, false},
		{"paid other modality", `{"text":{"input":{"price":0},"output":{"price":0}},"image":{"input":{"price":1}}}`, false},
		{"null cache", `{"text":{"input":{"price":0,"cache":null},"output":{"price":0}}}`, true},
		{"invalid json", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := freeCost(json.RawMessage(tc.cost)); got != tc.free {
				t.Fatalf("freeCost() = %v, want %v", got, tc.free)
			}
		})
	}
}
