# Eino

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-%3E%3D1.21-blue)](go.mod)

Eino is a fork of [cloudwego/eino](https://github.com/cloudwego/eino) — a powerful Go framework for building LLM-powered applications with composable, type-safe components.

> **Personal fork**: I'm using this to experiment with custom LLM integrations and learn the graph-based pipeline internals. Not intended for production use.

## Overview

Eino provides a clean, extensible architecture for constructing AI pipelines, agents, and workflows. It emphasizes:

- **Type Safety**: Strongly typed component interfaces and data flow
- **Composability**: Mix and match components to build complex pipelines
- **Observability**: Built-in tracing and monitoring hooks
- **Extensibility**: Easy to add custom components and integrations

## Features

- 🔗 **Graph-based Pipelines**: Define complex data flows as directed graphs
- 🤖 **LLM Integrations**: Support for multiple LLM providers
- 🧰 **Tool Calling**: First-class support for function/tool calling
- 💾 **Memory Management**: Built-in conversation history and context management
- 🔄 **Streaming**: Native streaming support throughout the pipeline
- 📊 **Tracing**: OpenTelemetry-compatible observability

## Getting Started

### Prerequisites

- Go 1.21 or higher

### Installation

```bash
go get github.com/eino-project/eino
```

### Quick Example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/eino-project/eino/compose"
)

func main() {
    ctx := context.Background()

    // Build a simple chain
    chain, err := compose.NewChain[string, string]().
        AppendLambda(compose.InvokableLambda(func(ctx context.Context, input string) (string, error) {
            return "Processed: " + input, nil
        })).
        Compile(ctx)
    if err != nil {
        log.Fatal(err)
    }

    result, err := chain.Invoke(ctx, "Hello, Eino!")
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(result) // Output: Processed: Hello, Eino!
}
```

## Documentation

- [Architecture Overview](docs/architecture.md)
- [Component Guide](docs/components.md)
- [Pipeline Building](docs/pipelines.md)
- [Examples](examples/)

## Personal Notes

A few things I've found useful while digging into the codebase:

- The `compose` package is the best starting point — `Chain` covers most linear use cases before you need a full `Graph`.
- When debugging graph execution, setting `EINO_LOG_LEVEL=debug` in your environment gives verbose node-level output.
- Tool calling works well with OpenAI-compatible APIs; see `examples/tool_calling` for a minimal working setup.
- `Graph` vs `Chain`: use `Chain` for straight-line flows, reach for `Graph` only when you need branching or fan-out — the extra setup cost is real.
- Parallel node execution in a `Graph` is opt-in; nodes on independent branches run concurrently by default once the graph is compiled.
- Context cancellation propagates correctly through the entire graph — wrapping your root context with a timeout is a good habit to avoid hung pipelines during LLM calls.
