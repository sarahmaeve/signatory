# Quickstart

Clone signatory, evaluate `golang.org/x/mod`, and record a posture decision.

## Prerequisites

- Go 1.25+
- Git in PATH
- [Claude Code](https://claude.com/claude-code) or another MCP-capable client
- `GITHUB_TOKEN` in your environment — highly recommended; without it many GitHub signals come back empty
- macOS or Linux (Windows is not yet supported)

## 1. Clone and build

    git clone git@github.com:sarahmaeve/signatory.git
    cd signatory
    make install        # stamps version + commit into the binary
    signatory version   # smoke check

`go install` works too but skips the version stamp.

## 2. Local TLS trust (one-time)

    signatory certs check || signatory certs init

`/analyze` talks to a local pipeline service over HTTPS; this wires
`NODE_EXTRA_CA_CERTS` to the managed CA.

## 3. Launch Claude Code from the clone

    claude

The repo ships `.mcp.json` and `.claude/skills/` (`analyze`,
`vet-dependency`), so the MCP server and skills auto-load. If your `GOBIN`
is not `~/go/bin`, edit the `command` path in `.mcp.json`.

## 4. Evaluate and record

In Claude Code: `/analyze golang.org/x/mod`. Collectors fan out, analyst
agents dispatch, a synthesis lands in the store. Then in your shell:

    signatory summary golang.org/x/mod
    signatory show-analyses golang.org/x/mod

## Next

`signatory --help` for the full verb list. `design/vision.md` for the model.
`TROUBLESHOOTING.md` if anything broke.
