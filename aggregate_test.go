package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAggregateStream(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","created":2,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"He"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"look","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
		`data: [DONE]`,
	}, "\n\n")

	body, err := aggregateStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}

	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}

	if response["id"] != "chatcmpl-1" || response["object"] != "chat.completion" || response["created"] != float64(2) || response["model"] != "test" {
		t.Fatalf("response: %#v", response)
	}

	choices := asType[[]any](t, response["choices"])
	choice := asType[map[string]any](t, choices[0])

	message := asType[map[string]any](t, choice["message"])
	if choice["finish_reason"] != "tool_calls" || message["content"] != "Hello" {
		t.Fatalf("choice: %#v", choice)
	}

	calls := asType[[]any](t, message["tool_calls"])

	call := asType[map[string]any](t, calls[0])
	if len(calls) != 1 || call["id"] != "call_1" || call["type"] != "function" {
		t.Fatalf("tool calls: %#v", calls)
	}

	function := asType[map[string]any](t, call["function"])
	if function["name"] != "look" || function["arguments"] != `{"a":1}` {
		t.Fatalf("function: %#v", function)
	}

	if usage := asType[map[string]any](t, response["usage"]); usage["total_tokens"] != float64(5) {
		t.Fatalf("usage: %#v", usage)
	}
}

func TestAggregateStreamDefaults(t *testing.T) {
	t.Parallel()

	body, err := aggregateStream(strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	if err != nil {
		t.Fatal(err)
	}

	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}

	choice := asType[map[string]any](t, asType[[]any](t, response["choices"])[0])
	if choice["finish_reason"] != "stop" || asType[map[string]any](t, choice["message"])["content"] != "hi" {
		t.Fatalf("choice: %#v", choice)
	}

	if _, ok := response["usage"]; ok {
		t.Fatalf("unexpected usage: %#v", response)
	}
}

func TestAggregateStreamRejectsEmptyAndErrorChunks(t *testing.T) {
	t.Parallel()

	if _, err := aggregateStream(strings.NewReader("event: ping\n\n")); err == nil {
		t.Fatal("accepted an empty stream")
	}

	_, err := aggregateStream(strings.NewReader("data: {\"error\":{\"message\":\"boom\"}}\n\n"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error chunk: %v", err)
	}
}
