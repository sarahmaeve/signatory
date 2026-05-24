# widget-parser

[![Build Status](https://img.shields.io/github/actions/workflow/status/example/widget-parser/test.yml?branch=main)](https://github.com/example/widget-parser/actions)
[![Go Reference](https://pkg.go.dev/badge/github.com/example/widget-parser.svg)](https://pkg.go.dev/github.com/example/widget-parser)
[![License](https://img.shields.io/github/license/example/widget-parser)](LICENSE)

A small library for parsing widget configuration files.

## Installation

```
go get github.com/example/widget-parser
```

## Usage

```go
import "github.com/example/widget-parser"

parser := widget.New()
result, err := parser.Parse(content)
if err != nil {
    log.Fatal(err)
}
fmt.Println(result.WidgetCount)
```

## Configuration

The parser accepts a `Config` struct. See the [godoc](https://pkg.go.dev/github.com/example/widget-parser) for the full API.

## Contributing

Pull requests welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the development setup.

## License

MIT. See [LICENSE](LICENSE) for details.
