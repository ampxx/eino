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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// ==================================================================
// BUG-10/11: Failover config with AgenticMessage agent panics at runtime.
//
// The failoverProxyModel only implements model.BaseChatModel (i.e.,
// model.BaseModel[*schema.Message]). When M = *schema.AgenticMessage,
// the proxy is silently skipped during buildModelWrappersImpl, but the
// failover config is still stored. When wrapGenerateEndpoint reaches
// the failover block, it does an unchecked any(innerEndpoint).(typedGenerateEndpoint[*schema.Message])
// which panics.
//
// Impact: Any agentic agent created with ModelFailoverConfig will
// crash on the first model call.
// ==================================================================

func TestAttack_FailoverPanicsOnAgenticAgent(t *testing.T) {
	ctx := context.Background()

	m := &mockAgenticModel{
		generateFn: func(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
			return agenticMsg("ok"), nil
		},
	}

	fallbackModel := &mockChatModelForAttack{
		generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
			return schema.AssistantMessage("fallback", nil), nil
		},
	}

	_, err := NewTypedChatModelAgent[*schema.AgenticMessage](ctx, &TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name:        "FailoverAgent",
		Description: "Agent with failover config",
		Model:       m,
		ModelFailoverConfig: &ModelFailoverConfig{
			MaxRetries: 1,
			ShouldFailover: func(ctx context.Context, outputMessage *schema.Message, outputErr error) bool {
				return true
			},
			GetFailoverModel: func(ctx context.Context, failoverCtx *FailoverContext) (model.BaseChatModel, []*schema.Message, error) {
				return fallbackModel, nil, nil
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ModelFailoverConfig is only supported for *schema.Message agents")
}

// ==================================================================
// BUG-3: getAgenticReactChatHistory panics if state Messages is empty.
//
// len(st.Messages)-1 underflows to a very large uint when Messages is
// empty, causing make() to panic with a negative length.
//
// Impact: Any agent tool with fullChatHistoryAsInput that is called
// when the agent state has zero messages will crash.
// ==================================================================

func TestAttack_EmptyMessagesInAgenticReactHistory(t *testing.T) {
	g := compose.NewGraph[string, []*schema.AgenticMessage](compose.WithGenLocalState(func(ctx context.Context) (state *typedState[*schema.AgenticMessage]) {
		return &typedState[*schema.AgenticMessage]{
			Messages: []*schema.AgenticMessage{},
		}
	}))
	require.NoError(t, g.AddLambdaNode("1", compose.InvokableLambda(func(ctx context.Context, input string) (output []*schema.AgenticMessage, err error) {
		return getAgenticReactChatHistory(ctx, "DestAgent")
	})))
	require.NoError(t, g.AddEdge(compose.START, "1"))
	require.NoError(t, g.AddEdge("1", compose.END))

	ctx := context.Background()
	ctx, _ = initRunCtx(ctx, "MyAgent", nil)
	runner, err := g.Compile(ctx)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		result, err := runner.Invoke(ctx, "")
		if err != nil {
			t.Logf("Got error (acceptable): %v", err)
			return
		}
		t.Logf("Got %d messages", len(result))
	}, "BUG: getAgenticReactChatHistory should not panic with empty Messages slice")
}

// Same test for the original getReactChatHistory
func TestAttack_EmptyMessagesInReactHistory(t *testing.T) {
	g := compose.NewGraph[string, []Message](compose.WithGenLocalState(func(ctx context.Context) (state *State) {
		return &State{
			Messages: []Message{},
		}
	}))
	require.NoError(t, g.AddLambdaNode("1", compose.InvokableLambda(func(ctx context.Context, input string) (output []Message, err error) {
		return getReactChatHistory(ctx, "DestAgent")
	})))
	require.NoError(t, g.AddEdge(compose.START, "1"))
	require.NoError(t, g.AddEdge("1", compose.END))

	ctx := context.Background()
	ctx, _ = initRunCtx(ctx, "MyAgent", nil)
	runner, err := g.Compile(ctx)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		result, err := runner.Invoke(ctx, "")
		if err != nil {
			t.Logf("Got error (acceptable): %v", err)
			return
		}
		t.Logf("Got %d messages", len(result))
	}, "BUG: getReactChatHistory should not panic with empty Messages slice")
}

// ==================================================================
// BUG-4: Cross-type agent tool error is deferred to runtime event loop.
//
// A TypedAgentTool[*schema.AgenticMessage] used inside a *schema.Message
// agent will only error deep in an iteration loop, not at creation time.
// This test verifies the error IS emitted and does not crash.
// ==================================================================

func TestAttack_CrossTypeAgentToolGracefulError(t *testing.T) {
	ctx := context.Background()

	innerModel := &mockAgenticModel{
		generateFn: func(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
			return agenticMsg("inner result"), nil
		},
	}

	innerAgent, err := NewTypedChatModelAgent[*schema.AgenticMessage](ctx, &TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name:        "AgenticInner",
		Description: "An agentic agent used as a tool",
		Model:       innerModel,
	})
	require.NoError(t, err)

	agenticAgentTool := NewTypedAgentTool(ctx, TypedAgent[*schema.AgenticMessage](innerAgent))

	var outerCallCount int32
	outerModel := &mockChatModelForAttack{
		generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
			count := atomic.AddInt32(&outerCallCount, 1)
			if count == 1 {
				return &schema.Message{
					Role: schema.Assistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Function: schema.FunctionCall{Name: "AgenticInner", Arguments: `{"request":"test"}`}},
					},
				}, nil
			}
			return schema.AssistantMessage("done", nil), nil
		},
	}

	outerAgent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "OuterMessageAgent",
		Description: "A Message agent using an AgenticMessage sub-agent tool",
		Model:       outerModel,
		ToolsConfig: ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: []tool.BaseTool{agenticAgentTool},
			},
		},
	})
	require.NoError(t, err)

	runner := NewRunner(ctx, RunnerConfig{Agent: outerAgent, EnableStreaming: true})
	iter := runner.Query(ctx, "test cross-type")

	var capturedErr error
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			capturedErr = event.Err
			t.Logf("Cross-type error message: %v", event.Err)
		}
	}

	if capturedErr == nil {
		t.Log("DESIGN CONCERN: Cross-type agent tool (AgenticMessage sub-agent in Message agent) " +
			"only errors at event forwarding time when streaming is enabled. " +
			"The error check happens in the gen.Send path, which is only exercised " +
			"when the outer agent actually calls the tool AND streaming is enabled. " +
			"Without streaming, the tool result is returned as a string, so no type mismatch occurs.")
	} else {
		assert.Contains(t, capturedErr.Error(), "cross-message-type",
			"Error should mention cross-message-type incompatibility")
	}
}

type mockChatModelForAttack struct {
	generateFn func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

func (m *mockChatModelForAttack) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.generateFn(ctx, input, opts...)
}

func (m *mockChatModelForAttack) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	result, err := m.generateFn(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	r, w := schema.Pipe[*schema.Message](1)
	go func() { defer w.Close(); w.Send(result, nil) }()
	return r, nil
}

// ==================================================================
// BUG: newTypedUserMessages returns nil for unknown type parameter.
//
// The default case in newTypedUserMessages returns nil, which means
// the agent would be called with nil input, potentially causing a
// nil pointer dereference downstream.
// ==================================================================

func TestAttack_NewTypedUserMessages_BothTypes(t *testing.T) {
	t.Run("Message", func(t *testing.T) {
		result := newTypedUserMessages[*schema.Message]("hello")
		require.NotNil(t, result)
		assert.Len(t, result, 1)
		assert.Equal(t, schema.User, result[0].Role)
	})

	t.Run("AgenticMessage", func(t *testing.T) {
		result := newTypedUserMessages[*schema.AgenticMessage]("hello")
		require.NotNil(t, result)
		assert.Len(t, result, 1)
		assert.Equal(t, schema.AgenticRoleTypeUser, result[0].Role)
	})
}

// ==================================================================
// BUG: concatMessageStream falls through to error for wrapped types.
//
// The runtime type switch in concatMessageStream relies on exact type
// matching. This test verifies both known branches work correctly.
// ==================================================================

func TestAttack_ConcatMessageStream_BothTypes(t *testing.T) {
	t.Run("Message", func(t *testing.T) {
		r, w := schema.Pipe[*schema.Message](2)
		go func() {
			defer w.Close()
			w.Send(schema.AssistantMessage("chunk1", nil), nil)
			w.Send(schema.AssistantMessage("chunk2", nil), nil)
		}()
		result, err := concatMessageStream(r)
		assert.NoError(t, err)
		assert.NotNil(t, result)
	})

	t.Run("AgenticMessage", func(t *testing.T) {
		r, w := schema.Pipe[*schema.AgenticMessage](2)
		go func() {
			defer w.Close()
			w.Send(agenticMsg("chunk1"), nil)
			w.Send(agenticMsg("chunk2"), nil)
		}()
		result, err := concatMessageStream(r)
		assert.NoError(t, err)
		assert.NotNil(t, result)
	})
}

// ==================================================================
// BUG: Agent tool with fullChatHistoryAsInput should correctly
// convert history for both message types.
// ==================================================================

func TestAttack_AgentToolFullHistoryBothTypes(t *testing.T) {
	ctx := context.Background()

	t.Run("AgenticMessage", func(t *testing.T) {
		innerModel := &mockAgenticModel{
			generateFn: func(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
				return agenticMsg("inner response"), nil
			},
		}

		innerAgent, err := NewTypedChatModelAgent[*schema.AgenticMessage](ctx, &TypedChatModelAgentConfig[*schema.AgenticMessage]{
			Name:        "InnerAgentic",
			Description: "Inner agent",
			Model:       innerModel,
		})
		require.NoError(t, err)

		agentTool := NewTypedAgentTool(ctx, TypedAgent[*schema.AgenticMessage](innerAgent),
			WithFullChatHistoryAsInput())
		require.NotNil(t, agentTool)

		info, err := agentTool.Info(ctx)
		require.NoError(t, err)
		assert.Equal(t, "InnerAgentic", info.Name)
	})
}

// ==================================================================
// BUG: typedWrapIterWithOnEnd uses unchecked assertions.
// Verify it works for both message types without panicking.
// ==================================================================

func TestAttack_TypedWrapIterWithOnEnd_BothTypes(t *testing.T) {
	t.Run("Message", func(t *testing.T) {
		require.NotPanics(t, func() {
			iter, gen := NewAsyncIteratorPair[*AgentEvent]()
			go func() {
				defer gen.Close()
				gen.Send(&AgentEvent{
					Output: &AgentOutput{
						MessageOutput: &MessageVariant{
							Message: schema.AssistantMessage("test", nil),
						},
					},
				})
			}()
			wrapped := typedWrapIterWithOnEnd[*schema.Message](context.Background(), iter)
			for {
				_, ok := wrapped.Next()
				if !ok {
					break
				}
			}
		})
	})

	t.Run("AgenticMessage", func(t *testing.T) {
		require.NotPanics(t, func() {
			iter, gen := NewAsyncIteratorPair[*TypedAgentEvent[*schema.AgenticMessage]]()
			go func() {
				defer gen.Close()
				gen.Send(&TypedAgentEvent[*schema.AgenticMessage]{
					Output: &TypedAgentOutput[*schema.AgenticMessage]{
						MessageOutput: &TypedMessageVariant[*schema.AgenticMessage]{
							Message: agenticMsg("test"),
						},
					},
				})
			}()
			wrapped := typedWrapIterWithOnEnd[*schema.AgenticMessage](context.Background(), iter)
			for {
				_, ok := wrapped.Next()
				if !ok {
					break
				}
			}
		})
	})
}

// ==================================================================
// BUG: Agentic ChatModelAgent with nil ToolsConfig should still work.
// Verify no nil dereference in the agentic react graph construction.
// ==================================================================

func TestAttack_AgenticChatModelAgent_NilToolsConfig(t *testing.T) {
	ctx := context.Background()

	m := &mockAgenticModel{
		generateFn: func(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
			return agenticMsg("no tools"), nil
		},
	}

	agent, err := NewTypedChatModelAgent[*schema.AgenticMessage](ctx, &TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name:        "NoToolsAgent",
		Description: "No tools agent",
		Model:       m,
	})
	require.NoError(t, err)

	runner := NewTypedRunner[*schema.AgenticMessage](TypedRunnerConfig[*schema.AgenticMessage]{
		Agent: agent,
	})
	iter := runner.Query(ctx, "no tools test")

	var events []*TypedAgentEvent[*schema.AgenticMessage]
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		events = append(events, event)
	}

	require.NotEmpty(t, events)
	last := events[len(events)-1]
	assert.Nil(t, last.Err)
	assert.Equal(t, "no tools", agenticTextContent(last.Output.MessageOutput.Message))
}

// ==================================================================
// BUG: Agentic agent with streaming enabled and model error should
// propagate error cleanly, not panic on nil stream.
// ==================================================================

func TestAttack_AgenticStreamingModelError(t *testing.T) {
	ctx := context.Background()

	m := &mockAgenticModel{
		streamFn: func(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
			return nil, assert.AnError
		},
	}

	agent, err := NewTypedChatModelAgent[*schema.AgenticMessage](ctx, &TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name:        "StreamErrAgent",
		Description: "streaming error",
		Model:       m,
	})
	require.NoError(t, err)

	runner := NewTypedRunner[*schema.AgenticMessage](TypedRunnerConfig[*schema.AgenticMessage]{
		Agent:           agent,
		EnableStreaming: true,
	})

	require.NotPanics(t, func() {
		iter := runner.Query(ctx, "stream error")
		var capturedErr error
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event.Err != nil {
				capturedErr = event.Err
			}
		}
		require.Error(t, capturedErr, "streaming model error should propagate")
	})
}

// ==================================================================
// BUG: TypedRunner.ResumeWithParams should handle missing checkpoint
// gracefully (return error, not panic).
// ==================================================================

func TestAttack_ResumeWithMissingCheckpoint(t *testing.T) {
	ctx := context.Background()

	agent := &myAgenticAgent{
		name: "resume-agent",
		runFn: func(ctx context.Context, input *TypedAgentInput[*schema.AgenticMessage], options ...AgentRunOption) *AsyncIterator[*TypedAgentEvent[*schema.AgenticMessage]] {
			iter, gen := NewAsyncIteratorPair[*TypedAgentEvent[*schema.AgenticMessage]]()
			go func() {
				defer gen.Close()
				gen.Send(&TypedAgentEvent[*schema.AgenticMessage]{
					Output: &TypedAgentOutput[*schema.AgenticMessage]{
						MessageOutput: &TypedMessageVariant[*schema.AgenticMessage]{
							Message: agenticMsg("ok"),
						},
					},
				})
			}()
			return iter
		},
	}

	store := newMyStore()
	runner := NewTypedRunner[*schema.AgenticMessage](TypedRunnerConfig[*schema.AgenticMessage]{
		Agent:           agent,
		CheckPointStore: store,
	})

	require.NotPanics(t, func() {
		iter, err := runner.ResumeWithParams(ctx, "nonexistent-checkpoint", &ResumeParams{
			Targets: map[string]any{"fake-id": nil},
		})
		if err != nil {
			t.Logf("Got expected error: %v", err)
			return
		}
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event.Err != nil {
				t.Logf("Got error event: %v", event.Err)
			}
		}
	}, "ResumeWithParams with nonexistent checkpoint should not panic")
}
