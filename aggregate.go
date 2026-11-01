// Package main provides stream aggregation and request routing.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// aggregateStream turns an OpenAI-compatible event stream into a single chat
// completion. The keyless free tier only accepts streaming requests, so the
// gateway uses this to keep the non-streaming API contract for callers.
func aggregateStream(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	state := completionState{choices: make(map[int]*completionChoice)}
	seen := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}

		if data == "[DONE]" {
			break
		}

		var chunk completionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("decode stream chunk: %w", err)
		}

		if len(chunk.Error) > 0 && string(chunk.Error) != jsonNull {
			return nil, fmt.Errorf("upstream stream error: %s", chunk.Error)
		}

		seen = true

		state.merge(chunk)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	if !seen {
		return nil, errors.New("upstream returned an empty stream")
	}

	return state.response()
}

type completionChunk struct {
	ID                string          `json:"id"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	SystemFingerprint string          `json:"system_fingerprint"`
	Choices           []chunkChoice   `json:"choices"`
	Usage             json.RawMessage `json:"usage"`
	Error             json.RawMessage `json:"error"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []chunkToolCall `json:"tool_calls"`
}

type chunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type completionState struct {
	id                string
	created           int64
	model             string
	systemFingerprint string
	usage             json.RawMessage
	choices           map[int]*completionChoice
}

type completionChoice struct {
	content     strings.Builder
	reasoning   strings.Builder
	finish      string
	toolCalls   map[int]*completionToolCall
	toolCallIDs []int
}

type completionToolCall struct {
	id        string
	typ       string
	name      strings.Builder
	arguments strings.Builder
}

func (s *completionState) merge(chunk completionChunk) {
	s.mergeMetadata(chunk)

	for _, delta := range chunk.Choices {
		choice := s.choices[delta.Index]
		if choice == nil {
			choice = &completionChoice{toolCalls: make(map[int]*completionToolCall)}
			s.choices[delta.Index] = choice
		}

		choice.merge(delta)
	}
}

func (s *completionState) mergeMetadata(chunk completionChunk) {
	if chunk.ID != "" {
		s.id = chunk.ID
	}

	if chunk.Created != 0 {
		s.created = chunk.Created
	}

	if chunk.Model != "" {
		s.model = chunk.Model
	}

	if chunk.SystemFingerprint != "" {
		s.systemFingerprint = chunk.SystemFingerprint
	}

	if len(chunk.Usage) > 0 && string(chunk.Usage) != jsonNull {
		s.usage = chunk.Usage
	}
}

func (c *completionChoice) merge(delta chunkChoice) {
	c.content.WriteString(delta.Delta.Content)
	c.reasoning.WriteString(delta.Delta.ReasoningContent)

	for _, call := range delta.Delta.ToolCalls {
		tool := c.toolCalls[call.Index]
		if tool == nil {
			tool = &completionToolCall{}
			c.toolCalls[call.Index] = tool
			c.toolCallIDs = append(c.toolCallIDs, call.Index)
		}

		if call.ID != "" {
			tool.id = call.ID
		}

		if call.Type != "" {
			tool.typ = call.Type
		}

		tool.name.WriteString(call.Function.Name)
		tool.arguments.WriteString(call.Function.Arguments)
	}

	if delta.FinishReason != nil {
		c.finish = *delta.FinishReason
	}
}

func (s *completionState) response() ([]byte, error) {
	indexes := make([]int, 0, len(s.choices))
	for index := range s.choices {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	choices := make([]any, 0, len(indexes))
	for _, index := range indexes {
		choices = append(choices, s.choices[index].message(index))
	}

	response := map[string]any{
		"id":      s.id,
		"object":  "chat.completion",
		"created": s.created,
		"model":   s.model,
		"choices": choices,
	}
	if len(s.usage) > 0 {
		response["usage"] = s.usage
	}

	if s.systemFingerprint != "" {
		response["system_fingerprint"] = s.systemFingerprint
	}

	return json.Marshal(response)
}

func (c *completionChoice) message(index int) map[string]any {
	message := map[string]any{"role": "assistant"}

	switch {
	case c.content.Len() > 0:
		message["content"] = c.content.String()
	case len(c.toolCalls) > 0:
		message["content"] = nil
	default:
		message["content"] = ""
	}

	if c.reasoning.Len() > 0 {
		message["reasoning_content"] = c.reasoning.String()
	}

	if len(c.toolCalls) > 0 {
		calls := make([]any, 0, len(c.toolCalls))
		sort.Ints(c.toolCallIDs)

		for _, id := range c.toolCallIDs {
			tool := c.toolCalls[id]

			typ := tool.typ
			if typ == "" {
				typ = "function"
			}

			calls = append(calls, map[string]any{
				"id":   tool.id,
				"type": typ,
				"function": map[string]any{
					"name":      tool.name.String(),
					"arguments": tool.arguments.String(),
				},
			})
		}

		message["tool_calls"] = calls
	}

	finish := c.finish
	if finish == "" {
		if len(c.toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}

	return map[string]any{"index": index, "message": message, "finish_reason": finish}
}
