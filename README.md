# vault-mcp

A Go MCP server for a Markdown / Obsidian vault. One process shares a vault
between agents over Streamable HTTP, or serves one client over stdio. It uses
the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).
It never runs git commands. Version control belongs to an external process.

This is the first of three implementation stages for v0.1. It provides file
operations, vault rules, transport, and security. Markdown metadata, directory
summaries, and search follow in later PRs. There is no RAG or vector index.

## Build and run

Use Go 1.25.6 or newer. The build does not need a C compiler.

```sh
make build
./bin/vault-mcp --vault /path/to/Brain

# HTTP: export a secret into VAULT_MCP_TOKEN before starting.
./bin/vault-mcp --vault /path/to/Brain --transport http --listen 0.0.0.0:8080
```

The HTTP endpoint is `/mcp`. Each HTTP request needs
`Authorization: Bearer <token>` when a token is configured. Without a token,
the CLI only allows `127.0.0.1`. Use TLS at a reverse proxy for remote access;
the built-in listener uses plain HTTP. Logs go to stderr so stdout remains
available for the stdio protocol.

## Configuration

Flags override environment variables. Use `--help` for flag descriptions.

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--vault` | `VAULT_MCP_VAULT` | Required; existing vault directory |
| `--transport` | `VAULT_MCP_TRANSPORT` | `stdio` (`http` also supported) |
| `--listen` | `VAULT_MCP_LISTEN` | `127.0.0.1:8080` |
| `--token` | `VAULT_MCP_TOKEN` | Empty; prefer the environment variable |
| `--read-only` | `VAULT_MCP_READ_ONLY` | `false` |
| `--exclude` | `VAULT_MCP_EXCLUDE` | Empty; comma-separated doublestar globs |

Example: `--exclude 'raw/assets/**,private/**'`. Patterns and all tool paths
are relative to the vault and use `/`. Excluding a directory also excludes
its descendants. The rules file is subject to the same exclusions.

## Available tools (stage 1)

| Tool | Behavior |
| --- | --- |
| `read_note` | Read one UTF-8 `.md` file |
| `read_multiple_notes` | Read 1–50 notes; return an error per failed path |
| `get_vault_guide` | Return the current full rules file |
| `write_note` | `overwrite` (default), `append`, or `prepend`; create missing parents |
| `patch_note` | Replace a nonempty fragment that occurs exactly once |
| `delete_note` | Permanently delete one note |
| `move_note` | Move a note; return conservative wikilink references, including ambiguous matches |
| `move_file` | Move one file, including binary attachments |

Read-only mode registers only the first three tools. The storage layer also
rejects writes in this mode. Moves reject existing destinations and directories.
`move_note` leaves referring files unchanged. `move_file` does not scan backlinks.
Writes insert exactly the supplied content, with no added newline or separator.
Notes must be valid UTF-8 and at most 16 MiB, including the result of an append.

## Vault rules

At connection time the server reads the first accessible root file in this
order: `CLAUDE.md`, `AGENTS.md`, `README.md`. It includes that file in the MCP
initialization instructions. Instructions longer than 16,000 Unicode characters
are truncated and include a full-text hint. `get_vault_guide` and the resource
`vault://guide` return the full file, subject to the 16 MiB note limit.

The server reads rules again for each new connection. Existing sessions can
call the tool or resource to read updates. Removing a rules file activates the
next file in the priority order. If no file exists, the server returns a short
message explaining where to add one.

## Claude Code

Add this entry to your local MCP configuration, using absolute paths:

```json
{
  "mcpServers": {
    "brain": {
      "command": "/path/to/vault-mcp/bin/vault-mcp",
      "args": ["--vault", "/path/to/Brain"]
    }
  }
}
```

For a shared HTTP server use an entry with `"type": "http"`,
`"url": "https://vault.example/mcp"`, and
`"headers": {"Authorization": "Bearer <token>"}` instead of `command` and `args`.
Keep secrets out of committed configuration files.

## Multica Workspace MCP library

Save a server entry (without the outer `mcpServers` object) as a private file:

```json
{
  "url": "https://vault.example/mcp",
  "headers": {"Authorization": "Bearer <token>"}
}
```

```sh
multica workspace mcp add brain --server-config-file ./brain-mcp.json
```

This adds the server to the library. Assign it to the intended agents separately.
The URL must be reachable from each agent runtime. A stdio entry can use the
same `command` and `args` as above, provided those paths exist on that runtime.
Deployment and workspace changes are separate tasks.

## Security and concurrency

- All vault I/O uses `os.Root`. Absolute paths, `..` segments, hidden path
  components (including `.git` and `.obsidian`), and excluded paths are rejected.
- Symlinks are unsupported, even if they point within the vault. This also
  blocks aliases into hidden or excluded locations. Non-regular files are rejected.
- All write tools share one process-wide mutex per vault instance. Read-modify-write
  operations run inside the lock. Notes are staged in the same directory, synced,
  and renamed so readers see either the old or the new complete file.
- Run one server instance per vault. Separate stdio processes and external editors
  do not share the mutex. Coordinate external writes yourself. There is no git sync.
- Bearer authentication covers every HTTP method. Cross-origin requests are
  checked, and the SDK retains its localhost DNS rebinding protection.
- The token grants the tools' full access to allowed files. This is not a
  multi-user permission system. Keep the vault and its mount configuration under
  trusted local control. `os.Root` does not isolate bind mounts or hard links.
- Filesystem errors do not expose the vault's absolute host path.

## Development and checks

```sh
make check                  # race-enabled tests and go vet
make build                  # CGO_ENABLED=0 binary
go test ./...               # includes real stdio and HTTP MCP client tests
```

CI runs the checks on Linux and macOS. Tests cover traversal and symlink escapes,
hidden and excluded files, HTTP authentication, guide refresh, read-only tool
registration, atomic replacement, and ten concurrent appends from separate clients.
An optional test checks a disposable Brain copy without printing or committing
its content:

```sh
VAULT_MCP_ACCEPTANCE_VAULT=/path/to/Brain-copy go test ./internal/server -run TestBrainGuide -v
```

## References and license

Design references: [MCPVault](https://github.com/bitbonsai/mcpvault) and
[sage-wiki](https://github.com/xoai/sage-wiki) (path-security test scenarios).
No source code was copied from these projects. MIT license; see `LICENSE`.
