/*
 * Copyright 2025 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package adk

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func TestIsArgsComplete(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		expected bool
	}{
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"partial json", `{"name": "te`, false},
		{"valid json", `{"name": "test"}`, true},
		{"valid empty object", `{}`, true},
		{"non-json", `hello`, false},
		{"array", `["a","b"]`, false},
		{"valid nested", `{"a": {"b": 1}}`, true},
		{"partial nested", `{"a": {"b": 1}`, false},
		{"whitespace padded valid", `  {"x": 1}  `, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isArgsComplete(tt.args))
		})
	}
}

func TestToolCallAccumulator(t *testing.T) {
	t.Run("single chunk complete", func(t *testing.T) {
		acc := &toolCallAccumulator{}
		acc.merge(schema.ToolCall{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city": "NYC"}`,
			},
		})

		tc := acc.toToolCall()
		assert.Equal(t, "call-1", tc.ID)
		assert.Equal(t, "function", tc.Type)
		assert.Equal(t, "get_weather", tc.Function.Name)
		assert.Equal(t, `{"city": "NYC"}`, tc.Function.Arguments)
	})

	t.Run("multi chunk streaming", func(t *testing.T) {
		acc := &toolCallAccumulator{}
		acc.merge(schema.ToolCall{
			ID: "call-1",
			Function: schema.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"ci`,
			},
		})
		acc.merge(schema.ToolCall{
			Function: schema.FunctionCall{
				Arguments: `ty": "N`,
			},
		})
		acc.merge(schema.ToolCall{
			Function: schema.FunctionCall{
				Arguments: `YC"}`,
			},
		})

		tc := acc.toToolCall()
		assert.Equal(t, "call-1", tc.ID)
		assert.Equal(t, "get_weather", tc.Function.Name)
		assert.Equal(t, `{"city": "NYC"}`, tc.Function.Arguments)
	})
}

func TestDerefIndex(t *testing.T) {
	assert.Equal(t, 0, derefIndex(nil))
	idx := 5
	assert.Equal(t, 5, derefIndex(&idx))
}

func TestEagerCoordLifecycle(t *testing.T) {
	t.Run("store and collect", func(t *testing.T) {
		coord := newEagerCoord()
		coord.storeResult("call-1", &eagerToolResult{output: "result-1"})
		coord.storeResult("call-2", &eagerToolResult{
			enhancedOutput: &schema.ToolResult{
				Parts: []schema.ToolOutputPart{
					{Type: schema.ToolPartTypeText, Text: "enhanced"},
				},
			},
			useEnhanced: true,
		})
		coord.storeResult("call-3", &eagerToolResult{err: context.Canceled})
		coord.markDone()

		executed, enhanced, failed, err := coord.collectResults()
		assert.NoError(t, err)
		assert.Equal(t, map[string]string{"call-1": "result-1"}, executed)
		assert.Contains(t, enhanced, "call-2")
		assert.Equal(t, []string{"call-3"}, failed)
	})

	t.Run("aborted returns nil", func(t *testing.T) {
		coord := newEagerCoord()
		coord.storeResult("call-1", &eagerToolResult{output: "result-1"})
		coord.abort()
		coord.markDone()

		executed, enhanced, failed, err := coord.collectResults()
		assert.NoError(t, err)
		assert.Nil(t, executed)
		assert.Nil(t, enhanced)
		assert.Nil(t, failed)
	})

	t.Run("waitDone returns after markDone", func(t *testing.T) {
		coord := newEagerCoord()
		done := make(chan struct{})
		go func() {
			coord.waitDone(context.Background())
			close(done)
		}()

		select {
		case <-done:
			t.Fatal("should not be done yet")
		case <-time.After(50 * time.Millisecond):
		}

		coord.markDone()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("should have been done")
		}
	})

	t.Run("waitDone returns on context cancel", func(t *testing.T) {
		coord := newEagerCoord()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			coord.waitDone(ctx)
			close(done)
		}()

		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("should have returned on cancel")
		}
	})
}

func TestExtractToolCalls(t *testing.T) {
	t.Run("schema.Message", func(t *testing.T) {
		msg := &schema.Message{
			ToolCalls: []schema.ToolCall{
				{ID: "call-1", Function: schema.FunctionCall{Name: "tool1"}},
			},
		}
		tcs := extractToolCalls(msg)
		assert.Len(t, tcs, 1)
		assert.Equal(t, "call-1", tcs[0].ID)
	})

	t.Run("schema.Message no tool calls", func(t *testing.T) {
		msg := &schema.Message{Content: "hello"}
		tcs := extractToolCalls(msg)
		assert.Empty(t, tcs)
	})

	t.Run("schema.AgenticMessage", func(t *testing.T) {
		msg := &schema.AgenticMessage{
			ContentBlocks: []*schema.ContentBlock{
				{
					Type: schema.ContentBlockTypeFunctionToolCall,
					FunctionToolCall: &schema.FunctionToolCall{
						CallID:    "call-1",
						Name:      "tool1",
						Arguments: `{"a": 1}`,
					},
				},
				{
					Type:             schema.ContentBlockTypeAssistantGenText,
					AssistantGenText: &schema.AssistantGenText{Text: "hello"},
				},
			},
		}
		tcs := extractToolCalls(msg)
		assert.Len(t, tcs, 1)
		assert.Equal(t, "call-1", tcs[0].ID)
		assert.Equal(t, "tool1", tcs[0].Function.Name)
		assert.Equal(t, `{"a": 1}`, tcs[0].Function.Arguments)
	})
}

type eagerTestTool struct {
	name      string
	result    string
	delay     time.Duration
	callCount int32
}

func (t *eagerTestTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: t.name,
		Desc: "eager test tool",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"input": {Type: schema.String, Desc: "input"},
		}),
	}, nil
}

func (t *eagerTestTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	atomic.AddInt32(&t.callCount, 1)
	if t.delay > 0 {
		select {
		case <-time.After(t.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return t.result, nil
}

type eagerTestModel struct {
	mu        sync.Mutex
	callCount int
	responses []func(ctx context.Context, input []*schema.Message) (*schema.StreamReader[*schema.Message], error)
}

func (m *eagerTestModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	stream, err := m.Stream(ctx, input)
	if err != nil {
		return nil, err
	}
	return schema.ConcatMessageStream(stream)
}

func (m *eagerTestModel) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	idx := m.callCount
	m.callCount++
	m.mu.Unlock()

	if idx < len(m.responses) {
		return m.responses[idx](ctx, input)
	}

	return schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("done", nil),
	}), nil
}

func (m *eagerTestModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func streamWithToolCalls(toolCalls []schema.ToolCall) func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return func(_ context.Context, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray([]*schema.Message{
			schema.AssistantMessage("", toolCalls),
		}), nil
	}
}

func streamChunkedToolCall(id, name, args string) func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return func(_ context.Context, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		idx0 := 0
		chunks := []*schema.Message{
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{
					{Index: &idx0, ID: id, Function: schema.FunctionCall{Name: name, Arguments: args[:len(args)/2]}},
				},
			},
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{
					{Index: &idx0, Function: schema.FunctionCall{Arguments: args[len(args)/2:]}},
				},
			},
		}
		return schema.StreamReaderFromArray(chunks), nil
	}
}

func streamFinalAnswer(content string) func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return func(_ context.Context, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray([]*schema.Message{
			schema.AssistantMessage(content, nil),
		}), nil
	}
}

func TestEagerToolExecution_BasicFlow(t *testing.T) {
	ctx := context.Background()

	tool1 := &eagerTestTool{name: "tool1", result: "result1", delay: 50 * time.Millisecond}

	mdl := &eagerTestModel{
		responses: []func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error){
			streamWithToolCalls([]schema.ToolCall{
				{ID: "call-1", Function: schema.FunctionCall{Name: "tool1", Arguments: `{"input":"test"}`}},
			}),
			streamFinalAnswer("done"),
		},
	}

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "EagerTest",
		Description: "test agent",
		Model:       mdl,
		ToolsConfig: ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{tool1},
			},
			EagerExecution: true,
		},
	})
	require.NoError(t, err)

	input := &AgentInput{Messages: []Message{schema.UserMessage("test")}}
	iter := agent.Run(ctx, input)
	assert.NotNil(t, iter)

	var events []*AgentEvent
	for {
		evt, ok := iter.Next()
		if !ok {
			break
		}
		events = append(events, evt)
	}

	var hasOutput bool
	for _, evt := range events {
		if evt.Output != nil && evt.Output.MessageOutput != nil {
			hasOutput = true
			break
		}
	}
	assert.True(t, hasOutput, "should have final output")
	assert.Equal(t, int32(1), atomic.LoadInt32(&tool1.callCount), "tool should be called exactly once")
}

func TestEagerToolExecution_ChunkedArgs(t *testing.T) {
	ctx := context.Background()

	tool1 := &eagerTestTool{name: "tool1", result: "chunked-result"}

	mdl := &eagerTestModel{
		responses: []func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error){
			streamChunkedToolCall("call-1", "tool1", `{"input":"test"}`),
			streamFinalAnswer("done"),
		},
	}

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "EagerChunkedTest",
		Description: "test agent",
		Model:       mdl,
		ToolsConfig: ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{tool1},
			},
			EagerExecution: true,
		},
	})
	require.NoError(t, err)

	input := &AgentInput{Messages: []Message{schema.UserMessage("test")}}
	iter := agent.Run(ctx, input)

	var events []*AgentEvent
	for {
		evt, ok := iter.Next()
		if !ok {
			break
		}
		events = append(events, evt)
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&tool1.callCount))
}

func TestEagerToolExecution_ConcurrentTools(t *testing.T) {
	ctx := context.Background()

	tool1 := &eagerTestTool{name: "tool1", result: "result1", delay: 50 * time.Millisecond}
	tool2 := &eagerTestTool{name: "tool2", result: "result2", delay: 50 * time.Millisecond}

	mdl := &eagerTestModel{
		responses: []func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error){
			streamWithToolCalls([]schema.ToolCall{
				{ID: "call-1", Function: schema.FunctionCall{Name: "tool1", Arguments: `{"input":"a"}`}},
				{ID: "call-2", Function: schema.FunctionCall{Name: "tool2", Arguments: `{"input":"b"}`}},
			}),
			streamFinalAnswer("done"),
		},
	}

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "ConcurrentEagerTest",
		Description: "test agent",
		Model:       mdl,
		ToolsConfig: ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{tool1, tool2},
			},
			EagerExecution: true,
		},
	})
	require.NoError(t, err)

	start := time.Now()
	input := &AgentInput{Messages: []Message{schema.UserMessage("test")}}
	iter := agent.Run(ctx, input)

	for {
		_, ok := iter.Next()
		if !ok {
			break
		}
	}

	elapsed := time.Since(start)
	assert.Equal(t, int32(1), atomic.LoadInt32(&tool1.callCount))
	assert.Equal(t, int32(1), atomic.LoadInt32(&tool2.callCount))
	assert.Less(t, elapsed, 300*time.Millisecond, "concurrent tools should not take much longer than a single tool")
}

func TestEagerToolExecution_SequentialMode(t *testing.T) {
	ctx := context.Background()

	makeOrderTool := func(name, result string) *eagerTestTool {
		return &eagerTestTool{name: name, result: result}
	}

	tool1 := makeOrderTool("tool1", "r1")
	tool2 := makeOrderTool("tool2", "r2")

	mdl := &eagerTestModel{
		responses: []func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error){
			streamWithToolCalls([]schema.ToolCall{
				{ID: "call-1", Function: schema.FunctionCall{Name: "tool1", Arguments: `{"input":"a"}`}},
				{ID: "call-2", Function: schema.FunctionCall{Name: "tool2", Arguments: `{"input":"b"}`}},
			}),
			streamFinalAnswer("done"),
		},
	}

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "SequentialEagerTest",
		Description: "test agent",
		Model:       mdl,
		ToolsConfig: ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools:               []tool.BaseTool{tool1, tool2},
				ExecuteSequentially: true,
			},
			EagerExecution: true,
		},
	})
	require.NoError(t, err)

	input := &AgentInput{Messages: []Message{schema.UserMessage("test")}}
	iter := agent.Run(ctx, input)

	for {
		_, ok := iter.Next()
		if !ok {
			break
		}
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&tool1.callCount))
	assert.Equal(t, int32(1), atomic.LoadInt32(&tool2.callCount))
}

func TestEagerToolExecution_NoToolCalls(t *testing.T) {
	ctx := context.Background()

	mdl := &eagerTestModel{
		responses: []func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error){
			streamFinalAnswer("no tools needed"),
		},
	}

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "NoToolEagerTest",
		Description: "test agent",
		Model:       mdl,
		ToolsConfig: ToolsConfig{
			EagerExecution: true,
		},
	})
	require.NoError(t, err)

	input := &AgentInput{Messages: []Message{schema.UserMessage("test")}}
	iter := agent.Run(ctx, input)

	var hasOutput bool
	for {
		evt, ok := iter.Next()
		if !ok {
			break
		}
		if evt.Output != nil && evt.Output.MessageOutput != nil {
			hasOutput = true
			assert.Equal(t, "no tools needed", evt.Output.MessageOutput.Message.Content)
		}
	}
	assert.True(t, hasOutput)
}

func TestEagerToolExecution_DisabledByDefault(t *testing.T) {
	ctx := context.Background()

	tool1 := &eagerTestTool{name: "tool1", result: "result1"}

	mdl := &eagerTestModel{
		responses: []func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error){
			streamWithToolCalls([]schema.ToolCall{
				{ID: "call-1", Function: schema.FunctionCall{Name: "tool1", Arguments: `{"input":"test"}`}},
			}),
			streamFinalAnswer("done"),
		},
	}

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "DefaultTest",
		Description: "test agent",
		Model:       mdl,
		ToolsConfig: ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{tool1},
			},
		},
	})
	require.NoError(t, err)

	input := &AgentInput{Messages: []Message{schema.UserMessage("test")}}
	iter := agent.Run(ctx, input)

	for {
		_, ok := iter.Next()
		if !ok {
			break
		}
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&tool1.callCount))
}

func TestEagerToolExecution_GenerateMode(t *testing.T) {
	ctx := context.Background()

	tool1 := &eagerTestTool{name: "tool1", result: "result1"}

	mdl := &eagerTestModel{
		responses: []func(context.Context, []*schema.Message) (*schema.StreamReader[*schema.Message], error){
			streamWithToolCalls([]schema.ToolCall{
				{ID: "call-1", Function: schema.FunctionCall{Name: "tool1", Arguments: `{"input":"test"}`}},
			}),
			streamFinalAnswer("done"),
		},
	}

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "GenerateEagerTest",
		Description: "test agent",
		Model:       mdl,
		ToolsConfig: ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{tool1},
			},
			EagerExecution: true,
		},
	})
	require.NoError(t, err)

	input := &AgentInput{Messages: []Message{schema.UserMessage("test")}}
	iter := agent.Run(ctx, input)
	assert.NotNil(t, iter)

	for {
		_, ok := iter.Next()
		if !ok {
			break
		}
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&tool1.callCount))
}
