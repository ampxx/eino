/*
 * Copyright 2026 CloudWeGo Authors
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
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type failoverCurrentModelKey struct{}

type failoverCurrentModel struct {
	model any
}

func setFailoverCurrentModel(ctx context.Context, currentModel any) context.Context {
	return context.WithValue(ctx, failoverCurrentModelKey{}, &failoverCurrentModel{
		model: currentModel,
	})
}

func getFailoverCurrentModel(ctx context.Context) *failoverCurrentModel {
	if fm, ok := ctx.Value(failoverCurrentModelKey{}).(*failoverCurrentModel); ok {
		return fm
	}
	return nil
}

type failoverHasMoreAttemptsKey struct{}

// withFailoverHasMoreAttempts sets a flag in context indicating whether additional failover
// attempts remain after the current one. This is read by buildErrWrapper to decide whether
// stream errors should be wrapped as WillRetryError.
func withFailoverHasMoreAttempts(ctx context.Context, hasMore bool) context.Context {
	return context.WithValue(ctx, failoverHasMoreAttemptsKey{}, hasMore)
}

// getFailoverHasMoreAttempts returns true if the current failover attempt has more attempts
// after it, false otherwise (including when no failover context is present).
func getFailoverHasMoreAttempts(ctx context.Context) bool {
	v, _ := ctx.Value(failoverHasMoreAttemptsKey{}).(bool)
	return v
}

type typedFailoverProxyModel[M MessageType] struct {
}

func (m *typedFailoverProxyModel[M]) prepareCallbacks(ctx context.Context) (context.Context, model.BaseModel[M], error) {
	current := getFailoverCurrentModel(ctx)
	if current == nil || current.model == nil {
		return nil, nil, errors.New("failover current model not found in context")
	}

	target, ok := current.model.(model.BaseModel[M])
	if !ok {
		return nil, nil, fmt.Errorf("failover current model type mismatch: expected model.BaseModel[%T], got %T", *new(M), current.model)
	}

	typ, _ := components.GetType(target)
	ctx = callbacks.EnsureRunInfo(ctx, typ, components.ComponentOfChatModel)

	if !components.IsCallbacksEnabled(target) {
		target = typedCallbackInjectionModelWrapper[M]{}.wrapModel(target)
	}

	return ctx, target, nil
}

func (m *typedFailoverProxyModel[M]) Generate(ctx context.Context, input []M, opts ...model.Option) (M, error) {
	nCtx, target, err := m.prepareCallbacks(ctx)
	if err != nil {
		var zero M
		return zero, err
	}

	ctx = callbacks.OnStart(ctx, input)

	result, err := target.Generate(nCtx, input, opts...)
	if err != nil {
		callbacks.OnError(ctx, err)
		return result, err
	}

	callbacks.OnEnd(ctx, result)

	return result, nil
}

func (m *typedFailoverProxyModel[M]) Stream(ctx context.Context, input []M, opts ...model.Option) (*schema.StreamReader[M], error) {
	nCtx, target, err := m.prepareCallbacks(ctx)
	if err != nil {
		return nil, err
	}

	ctx = callbacks.OnStart(ctx, input)

	result, err := target.Stream(nCtx, input, opts...)
	if err != nil {
		callbacks.OnError(ctx, err)
		return nil, err
	}

	_, wrappedStream := callbacks.OnEndWithStreamOutput(ctx, result)
	return wrappedStream, nil
}

func (m *typedFailoverProxyModel[M]) IsCallbacksEnabled() bool {
	return true
}

func (m *typedFailoverProxyModel[M]) GetType() string {
	return "FailoverProxyModel"
}

type failoverProxyModel = typedFailoverProxyModel[*schema.Message]

// FailoverContext contains context information during failover process.
type FailoverContext struct {
	// FailoverAttempt is the current failover attempt number, starting from 1.
	FailoverAttempt uint

	// InputMessages is the original input messages before any transformation.
	InputMessages []*schema.Message

	// LastOutputMessage is the output message from the last failed attempt.
	// May be nil if no output was produced. For streaming, this may be a partial message
	// already received before the stream error.
	LastOutputMessage *schema.Message

	// LastErr is the error from the last failed attempt that triggered this failover.
	//
	// Note: When ModelRetryConfig is also configured, LastErr will be a *RetryExhaustedError
	// (if retries were exhausted) rather than the original model error. The original error
	// can be retrieved via RetryExhaustedError.LastErr.
	LastErr error
}

// ModelFailoverConfig configures failover behavior for ChatModel.
// When configured, each ChatModel call first tries the last successful model (initially the configured Model),
// and if that fails, calls GetFailoverModel to select alternate models.
type ModelFailoverConfig struct {
	// MaxRetries specifies the maximum number of failover attempts.
	//
	// When failover is triggered, GetFailoverModel will be called up to MaxRetries times
	// (FailoverAttempt starts from 1). If GetFailoverModel returns an error, failover
	// stops immediately and that error is returned.
	//
	// A value of 0 means no failover (GetFailoverModel will not be called).
	// A value of 1 means GetFailoverModel may be called once.
	//
	// Note: if lastSuccessModel is set (from a previous successful call), it will be tried
	// first before calling GetFailoverModel.
	MaxRetries uint

	// ShouldFailover determines whether to fail over to the next model when an error occurs.
	// It receives the output message (may be nil if no output is available) and the error (non-nil on failure).
	// For streaming errors, outputMessage can carry a partial message accumulated before the error.
	//
	// Note: When ModelRetryConfig is also configured, outputErr will be a *RetryExhaustedError
	// (if retries were exhausted) rather than the original model error. Use errors.As to extract
	// the RetryExhaustedError and access RetryExhaustedError.LastErr for the original error:
	//
	//   var retryErr *adk.RetryExhaustedError
	//   if errors.As(outputErr, &retryErr) {
	//       // retryErr.LastErr contains the original model error
	//   }
	//
	// Note: When the context itself is cancelled (ctx.Err() != nil), failover will stop immediately
	// regardless of this function. However, if the model returns context.Canceled or context.DeadlineExceeded
	// as an error while the context is still active, this function will still be called.
	// Should not be nil when ModelFailoverConfig is set.
	// Return true to fail over to the next model, false to stop and return the current result/error.
	ShouldFailover func(ctx context.Context, outputMessage *schema.Message, outputErr error) bool

	// GetFailoverModel is called when a model call fails and ShouldFailover returns true.
	// It selects the next model to use for the failover attempt and optionally transforms input messages.
	// It receives the failover context containing attempt number (starting from 1), original input, and last result.
	// Return values:
	//   - failoverModel: The model to use for this failover attempt.
	//   - failoverModelInputMessages: The transformed input messages for the failover model. If nil, will use original input.
	//   - failoverErr: If non-nil, failover stops and this error is returned.
	// Should not be nil when ModelFailoverConfig is set via ChatModelAgentConfig.
	GetFailoverModel func(ctx context.Context, failoverCtx *FailoverContext) (
		failoverModel model.BaseChatModel, failoverModelInputMessages []*schema.Message, failoverErr error)
}

func getLastSuccessModel(ctx context.Context) any {
	if execCtx := getChatModelAgentExecCtx(ctx); execCtx != nil {
		return execCtx.failoverLastSuccessModel
	}
	return nil
}

func typedGetLastSuccessModel[M MessageType](ctx context.Context) model.BaseModel[M] {
	execCtx := getTypedChatModelAgentExecCtx[M](ctx)
	if execCtx == nil {
		return nil
	}
	m := execCtx.failoverLastSuccessModel
	if m == nil {
		return nil
	}
	if typed, ok := m.(model.BaseModel[M]); ok {
		return typed
	}
	return nil
}

func setLastSuccessModel(ctx context.Context, m any) {
	if execCtx := getChatModelAgentExecCtx(ctx); execCtx != nil {
		execCtx.failoverLastSuccessModel = m
	}
}

func typedSetLastSuccessModel[M MessageType](ctx context.Context, m model.BaseModel[M]) {
	if execCtx := getTypedChatModelAgentExecCtx[M](ctx); execCtx != nil {
		execCtx.failoverLastSuccessModel = m
	}
}

type typedFailoverModelWrapper[M MessageType] struct {
	config *ModelFailoverConfig
	inner  model.BaseModel[M]
}

type failoverModelWrapper = typedFailoverModelWrapper[*schema.Message]

func newTypedFailoverModelWrapper[M MessageType](inner model.BaseModel[M], config *ModelFailoverConfig) *typedFailoverModelWrapper[M] {
	return &typedFailoverModelWrapper[M]{
		config: config,
		inner:  inner,
	}
}

func newFailoverModelWrapper(inner model.BaseChatModel, config *ModelFailoverConfig) *failoverModelWrapper {
	return newTypedFailoverModelWrapper[*schema.Message](inner, config)
}

func toSchemaMessage[M MessageType](msg M) *schema.Message {
	if m, ok := any(msg).(*schema.Message); ok {
		return m
	}
	return nil
}

func toSchemaMessages[M MessageType](msgs []M) []*schema.Message {
	if m, ok := any(msgs).([]*schema.Message); ok {
		return m
	}
	return nil
}

func (f *typedFailoverModelWrapper[M]) needFailover(ctx context.Context, outputMessage M, outputErr error) bool {
	if ctx.Err() != nil {
		return false
	}

	return f.config.ShouldFailover(ctx, toSchemaMessage(outputMessage), outputErr)
}

func (f *typedFailoverModelWrapper[M]) getFailoverModel(ctx context.Context, failoverCtx *FailoverContext) (model.BaseModel[M], []M, error) {
	chatModel, msgs, err := f.config.GetFailoverModel(ctx, failoverCtx)
	if err != nil {
		return nil, nil, err
	}
	if chatModel == nil {
		return nil, nil, nil
	}

	typedModel, ok := any(chatModel).(model.BaseModel[M])
	if !ok {
		return nil, nil, fmt.Errorf("failover GetFailoverModel returned model of type %T, expected model.BaseModel[%T]", chatModel, *new(M))
	}

	var typedMsgs []M
	if msgs != nil {
		if m, ok := any(msgs).([]M); ok {
			typedMsgs = m
		}
	}

	return typedModel, typedMsgs, nil
}

func (f *typedFailoverModelWrapper[M]) Generate(ctx context.Context, input []M, opts ...model.Option) (M, error) {
	if f.config.GetFailoverModel == nil {
		return f.inner.Generate(ctx, input, opts...)
	}

	var lastOutputMessage M
	var lastErr error

	if lastSuccess := typedGetLastSuccessModel[M](ctx); lastSuccess != nil {
		if err := ctx.Err(); err != nil {
			var zero M
			return zero, err
		}

		modelCtx := setFailoverCurrentModel(ctx, lastSuccess)
		modelCtx = withFailoverHasMoreAttempts(modelCtx, f.config.MaxRetries > 0)
		result, err := f.inner.Generate(modelCtx, input, opts...)
		if err == nil {
			return result, nil
		}

		lastOutputMessage = result
		lastErr = err

		if !f.needFailover(ctx, result, err) {
			return result, err
		}

		log.Printf("failover ChatModel.Generate lastSuccessModel failed: %v", err)
	}

	for attempt := uint(1); attempt <= f.config.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			var zero M
			return zero, err
		}

		failoverCtx := &FailoverContext{
			FailoverAttempt:   attempt,
			InputMessages:     toSchemaMessages(input),
			LastOutputMessage: toSchemaMessage(lastOutputMessage),
			LastErr:           lastErr,
		}

		currentModel, currentInput, err := f.getFailoverModel(ctx, failoverCtx)
		if err != nil {
			var zero M
			return zero, err
		}
		if currentModel == nil {
			var zero M
			return zero, fmt.Errorf("failover GetFailoverModel returned nil model at attempt %d", attempt)
		}

		if currentInput == nil {
			currentInput = input
		}

		modelCtx := setFailoverCurrentModel(ctx, currentModel)
		modelCtx = withFailoverHasMoreAttempts(modelCtx, attempt < f.config.MaxRetries)
		result, err := f.inner.Generate(modelCtx, currentInput, opts...)
		lastOutputMessage = result
		lastErr = err

		if err == nil {
			typedSetLastSuccessModel[M](ctx, currentModel)
			return result, nil
		}

		if !f.needFailover(ctx, result, err) {
			return result, err
		}

		if attempt < f.config.MaxRetries {
			log.Printf("failover ChatModel.Generate attempt %d failed: %v", attempt, err)
		}
	}

	return lastOutputMessage, lastErr
}

func (f *typedFailoverModelWrapper[M]) Stream(ctx context.Context, input []M, opts ...model.Option) (
	*schema.StreamReader[M], error) {
	if f.config.GetFailoverModel == nil {
		return f.inner.Stream(ctx, input, opts...)
	}

	var lastOutputMessage M
	var lastErr error

	if lastSuccess := typedGetLastSuccessModel[M](ctx); lastSuccess != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		modelCtx := setFailoverCurrentModel(ctx, lastSuccess)
		modelCtx = withFailoverHasMoreAttempts(modelCtx, f.config.MaxRetries > 0)
		stream, err := f.inner.Stream(modelCtx, input, opts...)
		if err != nil {
			lastErr = err
			var zero M
			if !f.needFailover(ctx, zero, err) {
				return nil, err
			}
			log.Printf("failover ChatModel.Stream lastSuccessModel failed: %v", err)
		} else {
			copies := stream.Copy(2)
			checkCopy := copies[0]
			returnCopy := copies[1]

			outMsg, streamErr := typedConsumeStream(checkCopy)
			if streamErr != nil {
				lastOutputMessage = outMsg
				lastErr = streamErr
				returnCopy.Close()

				if !f.needFailover(ctx, outMsg, streamErr) {
					return nil, streamErr
				}
				log.Printf("failover ChatModel.Stream lastSuccessModel failed: %v", streamErr)
			} else {
				return returnCopy, nil
			}
		}
	}

	for attempt := uint(1); attempt <= f.config.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		failoverCtx := &FailoverContext{
			FailoverAttempt:   attempt,
			InputMessages:     toSchemaMessages(input),
			LastOutputMessage: toSchemaMessage(lastOutputMessage),
			LastErr:           lastErr,
		}

		currentModel, currentInput, err := f.getFailoverModel(ctx, failoverCtx)
		if err != nil {
			return nil, err
		}
		if currentModel == nil {
			return nil, fmt.Errorf("failover GetFailoverModel returned nil model at attempt %d", attempt)
		}

		if currentInput == nil {
			currentInput = input
		}

		modelCtx := setFailoverCurrentModel(ctx, currentModel)
		modelCtx = withFailoverHasMoreAttempts(modelCtx, attempt < f.config.MaxRetries)
		stream, err := f.inner.Stream(modelCtx, currentInput, opts...)
		if err != nil {
			lastErr = err
			var zero M
			lastOutputMessage = zero

			if !f.needFailover(ctx, zero, err) {
				return nil, err
			}

			if attempt < f.config.MaxRetries {
				log.Printf("failover ChatModel.Stream attempt %d failed: %v", attempt, err)
			}
			continue
		}

		copies := stream.Copy(2)
		checkCopy := copies[0]
		returnCopy := copies[1]

		outMsg, streamErr := typedConsumeStream(checkCopy)
		if streamErr != nil {
			lastOutputMessage = outMsg
			lastErr = streamErr
			returnCopy.Close()

			if !f.needFailover(ctx, outMsg, streamErr) {
				return nil, streamErr
			}

			if attempt < f.config.MaxRetries {
				log.Printf("failover ChatModel.Stream attempt %d failed: %v", attempt, streamErr)
			}
			continue
		}

		typedSetLastSuccessModel[M](ctx, currentModel)
		return returnCopy, nil
	}

	return nil, lastErr
}

func typedConsumeStream[M MessageType](stream *schema.StreamReader[M]) (M, error) {
	var zero M
	switch s := any(stream).(type) {
	case *schema.StreamReader[*schema.Message]:
		msg, err := consumeMessageStream(s)
		if msg != nil {
			return any(msg).(M), err
		}
		return zero, err
	case *schema.StreamReader[*schema.AgenticMessage]:
		msg, err := consumeAgenticStream(s)
		if msg != nil {
			return any(msg).(M), err
		}
		return zero, err
	default:
		return zero, fmt.Errorf("unsupported message type for stream consumption")
	}
}

func consumeMessageStream(stream *schema.StreamReader[*schema.Message]) (*schema.Message, error) {
	defer stream.Close()
	chunks := make([]*schema.Message, 0)
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			msg, _ := schema.ConcatMessages(chunks)
			return msg, err
		}

		chunks = append(chunks, chunk)
	}

	msg, _ := schema.ConcatMessages(chunks)
	return msg, nil
}

func consumeAgenticStream(stream *schema.StreamReader[*schema.AgenticMessage]) (*schema.AgenticMessage, error) {
	defer stream.Close()
	chunks := make([]*schema.AgenticMessage, 0)
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			msg, _ := schema.ConcatAgenticMessages(chunks)
			return msg, err
		}

		chunks = append(chunks, chunk)
	}

	msg, _ := schema.ConcatAgenticMessages(chunks)
	return msg, nil
}
