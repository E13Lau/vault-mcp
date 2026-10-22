# vault-mcp

A Go MCP server for a Markdown / Obsidian vault. One process shares a vault
between agents over Streamable HTTP, or serves one client over stdio. It uses
the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).
It never runs git commands. Version control belongs to an external process.

It provides 20 tools for files, Markdown metadata, layered summaries, links,
tags, and keyword search. There is no RAG or vector index.

## Install

Download `vault-mcp-<os>-<arch>` from the GitHub release (darwin or linux; amd64
or arm64), check it against `SHA256SUMS`, and `chmod +x` it. Or build from source:

Use Go 1.25.6 or newer. The build does not need a C compiler.

## Build and run

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
| `--index-file` | `VAULT_MCP_INDEX_FILE` | `index.md`; vault-relative summary index |
| `--deep-search-endpoint` | `VAULT_MCP_DEEP_SEARCH_ENDPOINT` | Empty; OpenRouter chat completions |
| `--deep-search-model` | `VAULT_MCP_DEEP_SEARCH_MODEL` | `deepseek/deepseek-v4-flash` |
| — | `VAULT_MCP_DEEP_SEARCH_API_KEY` | Empty; a key turns `deep_search` on. The key has no flag |

Example: `--exclude 'raw/assets/**,private/**'`. Patterns and all tool paths
are relative to the vault and use `/`. Excluding a directory also excludes
its descendants. The rules file is subject to the same exclusions.

## Available tools

| Tool | Behavior |
| --- | --- |
| `read_note` | Read one UTF-8 `.md` file |
| `read_multiple_notes` | Read 1–50 notes; return an error per failed path |
| `get_vault_guide` | Return the current full rules file |
| `get_note_outline` | L1: headings with levels and original line numbers; ignore fenced code |
| `read_note_lines` | L2: inclusive 1-based range, at most 2,000 lines; clamp end at EOF |
| `get_frontmatter` | Read YAML metadata; invalid YAML returns an error |
| `get_summary` | L0: summaries and sources for 1–50 paths |
| `list_directory` | Paginated notes and subdirectories with metadata and optional L0 summaries |
| `get_notes_info` | Metadata, size, modified time, line/link counts and backlinks for 1–50 paths |
| `get_vault_stats` | Accessible note, directory, attachment, byte, tag and wikilink counts |
| `resolve_wikilink` | Resolve shortest note paths; return all ambiguous candidates |
| `list_all_tags` | Frontmatter and inline tags, with note counts and paths |
| `search_notes` | BM25 search (any-word match, question-friendly) with Chinese support and path, tag and frontmatter filters |
| `deep_search` | Optional: re-rank BM25 candidates with a model judge. Registered only when a judge key is set |
| `write_note` | `overwrite` (default), `append`, or `prepend`; create missing parents |
| `patch_note` | Replace a nonempty fragment that occurs exactly once |
| `delete_note` | Permanently delete one note |
| `move_note` | Move a note; return indexed wikilink references, including ambiguous matches |
| `move_file` | Move one file, including binary attachments |
| `update_frontmatter` | Atomically add/replace `fields` and delete fields named in `remove` |
| `manage_tags` | Atomically `add`, `remove`, or `set` frontmatter tags; inline tags stay unchanged |

Read-only mode registers only the thirteen read tools. The storage layer also
rejects writes in this mode. Moves reject existing destinations and directories.
`move_note` leaves referring files unchanged. `move_file` does not scan backlinks.
Writes insert exactly the supplied content, with no added newline or separator.
Notes must be valid UTF-8 and at most 16 MiB, including the result of an append.

## Markdown, summaries, and browsing

Frontmatter updates use YAML nodes to locate changed top-level fields. All
other fields retain their original bytes, including quotes, spacing, comments,
field order, list formatting, and CRLF. The Markdown body stays byte-for-byte
unchanged. Invalid YAML or an update that leaves a dangling YAML alias is
rejected before writing. A flow-style **root** mapping (`{title: ..., tags: ...}`)
can be read but cannot be edited without reformatting; the tool returns an
error. Nested flow mappings and sequences in ordinary block-style frontmatter
are supported.

`get_summary` uses the first nonempty source in this order:

1. Frontmatter `summary`, then `description` (`source: "frontmatter"`).
2. Text after the note's wikilink in the configured index (`source: "index"`).
   Leading `—`, `-`, `:`, or `：` separators are removed.
3. The first body paragraph, limited to 120 Unicode characters (`source: "body"`).

The catalog parses notes once and checks modification time and size before
queries. Index links, summary text and resolved summaries are cached. Writes
invalidate affected entries; external edits are picked up on the next query.
No model is called. Non-UTF-8 or oversized notes are skipped by the text index
and reported in vault stats. Such a note does not prevent moving another note.
Invalid YAML is reported per note instead of silently replacing its metadata.

Example calls:

```json
{"name":"get_summary","arguments":{"paths":["wiki/concepts/example.md"]}}
{"name":"list_directory","arguments":{"path":"wiki/concepts","depth":1,"limit":50}}
{"name":"resolve_wikilink","arguments":{"link":"[[概念#标题|别名]]","source_path":"wiki/start.md"}}
{"name":"update_frontmatter","arguments":{"path":"note.md","fields":{"summary":"A short description"},"remove":["obsolete"]}}
{"name":"manage_tags","arguments":{"path":"note.md","action":"add","tags":["开发/iOS"]}}
```

Directory defaults are root path, depth 1, summaries included, and limit 100.
Depth is at most 10 and limit at most 500. A directory row includes its recursive
count of accessible Markdown files. Note titles use frontmatter `title`, falling
back to the filename without `.md`. Pass `next_cursor` back as `cursor` with
the same path, depth, and summary setting. A changed vault or directory makes
the cursor stale; restart pagination. Set `include_summary: false` to omit L0.

Wikilinks support `[[name]]`, `[[folder/name]]`, `[[name|alias]]`, `[[name#heading]]`,
`![[name]]`, and explicit `./` / `../` paths relative to `source_path`. Duplicate
filenames return every matching suffix candidate. Full vault-relative paths
take priority over suffix matches. Code and HTML comments do not create links
or tags. Tags support Chinese and `/` hierarchy. `manage_tags` edits only the
frontmatter field; use `list_all_tags` to see inline tags as well.

## Search

`search_notes` accepts keywords or a whole question. By default (`match: "any"`)
a note matches when it contains at least one query word, and results are ranked
by BM25 (k1 = 1.2, b = 0.75). A note with more of the query words ranks higher
(score × 1 + coverage, coverage = share of the query's IDF mass the note
contains). Common question words (哪些, 什么, 怎么, 如何, 是否, 有没有, 库里, 笔记,
页面, 相关, 记录, 吗, 呢, 的, 了 and a few English ones) are dropped from the
query. If nothing is left, the result is empty.

Ranking weights, all constants in `internal/search/search.go`: a title or
frontmatter `aliases` hit counts as 3 body hits, a tag or path hit as 2. A
keyword that appears whole adds +3 in the title and +1 in the body. The score
is also multiplied by `1 + 1.0 × ln(1 + backlinks)`, so well-linked concept and
entity pages beat the raw notes they summarize. Text in `"double quotes"` is a
phrase: it must occur in the title or body of every result.

`match: "all"` restores the v0.1 rule: every space-separated keyword must match
and stopwords are kept.

Text is lowercased. Latin and digit runs are words. Chinese, Japanese and Korean
text is indexed as single characters and adjacent pairs (bigrams), so no word
segmentation or dictionary is needed: `独立开发` matches the pairs `独立`, `立开`,
`开发`. A single-character query matches that character.

| Argument | Meaning |
| --- | --- |
| `query` | Keywords or a question; `"..."` marks a required phrase |
| `match` | `any` (default) or `all` |
| `path_prefix` | Only paths that start with this prefix, such as `wiki/` |
| `tag` | A tag; nested children (`dev` matches `dev/ios`) also match |
| `frontmatter` | Field/value pairs; a list field matches if it contains the value |
| `limit` | 1–100, default 20 |

Each result has `path`, `title`, `score`, `type`, `tags` and a `snippet` around the
first match. `total` counts all matches before `limit`. Nothing is indexed at
startup: the first query tokenizes the notes (about 2.4 s for 1,765 notes), and
later queries reuse the cached terms and only retokenize notes changed on disk.
Typical queries on that vault take 0.2–0.4 s. Excluded and hidden files are never searched.

## Deep search

`deep_search` is an optional second pass. It takes the same `query` as
`search_notes`, keeps the top 60 BM25 pages, cuts each page into chunks of at
most 8,000 characters at headings and blank lines, keeps the best chunk per
page, and sends those chunks to a model that scores each one from 0 to 1 for
whether it supplies a necessary fact in an answer chain (the criterion comes
from SAG2, MIT). Pages are ranked by their best chunk. `limit` is 1–20
(default 10) and applies to pages. `expansions` (0–2, default 0) adds frequent
terms from the best passages, runs BM25 again and scores the new pages.

The tool exists only when `VAULT_MCP_DEEP_SEARCH_API_KEY` is set. The key is
read from the environment only: there is no flag for it, and it is never
logged. The default endpoint is OpenRouter's chat completions API, so the
default judge is an OpenRouter model; `--deep-search-model` and
`--deep-search-endpoint` switch to another OpenAI-compatible provider.

One call costs one to two model requests, roughly 30k input tokens each, and
has a 15 s limit. The reply includes `usage` (`requests`, `input_tokens`,
`output_tokens`, `cost_usd`, `model`) when the provider reports it. When the
model fails, times out, or reports a bad answer, the tool returns the BM25
pages with `degraded: true` and a `note`; those scores are BM25 scores and carry
no model judgment. Nothing is written to the vault, and there is no vector
index.

The instructions sent to clients tell agents to call `search_notes` first and
to use `deep_search` only when keywords miss or the question uses different
words.

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

CI runs the checks on Linux and macOS, and cross-compiles darwin/linux for
amd64 and arm64. Pushing a tag such as `v0.1.0` publishes those four binaries
and a `SHA256SUMS` file as a GitHub release. Tests cover traversal and symlink escapes,
hidden and excluded files, HTTP authentication, guide refresh, read-only tool
registration, atomic replacement, and ten concurrent appends from separate clients.
An optional test checks a disposable Brain copy without printing or committing
its content:

```sh
VAULT_MCP_ACCEPTANCE_VAULT=/path/to/Brain-copy go test ./internal/server -run TestBrain -v
```

The copy must contain the root files and `wiki/concepts` for Markdown acceptance.
The test checks index-sourced summaries and pagination, then verifies frontmatter
priority on a temporary copy of a real note. Brain contents are never committed
or printed.

## References and license

Design references: [MCPVault](https://github.com/bitbonsai/mcpvault) and
[sage-wiki](https://github.com/xoai/sage-wiki) (path-security test scenarios).
No source code was copied from these projects. MIT license; see `LICENSE`.
