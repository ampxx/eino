# Eino

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-%3E%3D1.21-blue)](go.mod)

Eino is a fork of [cloudwego/eino](https://github.com/cloudwego/eino) — a powerful Go framework for building LLM-powered applications with composable, type-safe components.

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

## Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

1. Fork the repository
2. Create your feature branch (`git checkout -b feat/amazing-feature`)
3. Commit your changes following our [commit conventions](.github/.commit-rules.json)
4. Push to the branch (`git push origin feat/amazing-feature`)
5. Open a Pull Request using our [PR template](.github/PULL_REQUEST_TEMPLATE.md)

## License

This project is licensed under the Apache License 2.0 — see the [LICENSE](LICENSE) file for details.

## Acknowledgements

This project is a fork of [cloudwego/eino](https://github.com/cloudwego/eino). We are grateful to the original authors and contributors.
