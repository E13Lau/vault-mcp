# vault-mcp bench

Benchmark tooling for comparing MCP knowledge-base servers on a
Markdown vault. It measures retrieval quality (Recall@5, MRR@10),
end-to-end answer quality via a fixed answering model, write-task
behaviour, tool latency, and OpenRouter costs.

The tools are target-agnostic: everything they know about a specific
server lives in `internal/adapt` (which search tool to call and how to
read paths out of its result). Two targets are supported out of the
box:

- `vault` — [vault-mcp](..) over Streamable HTTP
- `sage` — [sage-wiki](https://github.com/xoai/sage-wiki) v0.2.11 over
  stdio (or its SSE/HTTP transport via a proxy)

## Privacy rules

This directory contains **only generic tooling**. It must never
contain vault content, question files, gold answers, result data, or
API keys:

- Question files, write-task files and result files are inputs/outputs
  passed by path; keep them outside any git repository.
- The OpenRouter API key is read **only** from an environment variable
  (`OPENROUTER_API_KEY` by default). Never write it into a file.
- Server logs are captured to `mcp-server.log` next to the results;
  they are for debugging and must not be committed either.
- Before pushing, inspect `git log -p bench/` and confirm no API
  key value appears in it.

## Build

```sh
cd bench
go build ./...
```

Go 1.25+; no third-party dependencies.

## Tools

### retrieval — search quality, no model

```sh
go run ./cmd/retrieval \
  -questions /path/to/questions.jsonl \
  -out /path/to/results/v \
  -target vault \
  -mcp-url http://127.0.0.1:8080/mcp
```

Calls only the search tool (default `search_notes` for vault,
`wiki_search` for sage) and writes `retrieval.json` + `retrieval.csv`
with per-question hit ranks and per-type Recall@5 / MRR@10.
Type E questions are skipped (they have no gold pages).

Flags worth knowing:

- `-limit 10` — number of results requested per query.
- `-search-text query|question` — what to send to the search tool.
  `query` uses the question's `query` field (keyword form, falling back
  to `question`); `question` uses the natural question text. Run both
  and report both: real agents often pass the whole question.
- `-prefix wiki/=_wiki/` — repeatable; maps a gold-page prefix to the
  prefix returned paths use, for systems that rewrite paths.
- `-channels bm25` — sage-wiki search channels (`bm25,vector,graph`;
  empty = server default).
- `-stdio-cmd "/path/server serve --transport stdio"` — use a stdio
  server instead of an HTTP URL. Exactly one of `-mcp-url` /
  `-stdio-cmd` must be given.

Scoring rules (see SCHEMA.md): `index.md` is dropped from results
before scoring, `related_pages` count for MRR but not for Recall.

### agent — end-to-end answers and write tasks, one fixed model

```sh
OPENROUTER_API_KEY=... go run ./cmd/agent \
  -questions /path/to/questions.jsonl \
  -out /path/to/results/v \
  -target vault \
  -mcp-url http://127.0.0.1:8080/mcp \
  -model deepseek/deepseek-v4-flash \
  -run-label v-run1 -phase agent -budget 0.6
```

The agent loop: a generic system prompt plus the usage instructions of
the system under test (vault: the MCP `initialize` instructions;
sage-wiki: a skill file produced by `sage-wiki skill refresh --target
generic`, passed with `-instructions <file>`). Every question runs the
same fixed parameters (`-temperature 0 -seed 42`, reasoning off via
`-reasoning none`), at most `-max-tool-calls` tool calls and
`-max-input-tokens` input tokens per question. `-runs 2` runs the whole
set twice with identical settings.

The agent only ever talks to the MCP server and OpenRouter; it has no
shell, filesystem or search access of its own.

Outputs:

- `agent.jsonl` — one line per question: answer, citations, tool-call
  count, prompt/completion tokens, cost, duration, error.
- `cost.jsonl` — one line per OpenRouter generation: generation id,
  model, token counts, USD cost, phase.

Cost control: `-budget` is a per-phase USD cap (checked against the
`-phase` entries in `cost.jsonl`); runs stop once it is reached.
`-tasks write_tasks.jsonl` runs write tasks through the same loop
(type `W` records in `agent.jsonl`).

Write-task post-checks (lint, git diff) are vault-specific and run
outside this tool; record the outcome next to the results.

### latency — tool call p50/p95

```sh
go run ./cmd/latency \
  -tool search_notes -args '{"query":"example"}' \
  -iterations 20 -cold 3 \
  -mcp-url http://127.0.0.1:8080/mcp -out /path/to/results/v
```

Cold samples open a fresh connection per measurement (initialize +
first call); warm samples repeat the call on one connection. Writes
`latency.json` + `latency.csv`.

### report — per-configuration summary

```sh
go run ./cmd/report -results /path/to/results/v -out /path/to/v.summary.json
```

Aggregates a results directory into one JSON summary (retrieval
totals, latency, agent line count, cost ledger).

## File formats

See [SCHEMA.md](SCHEMA.md). Sample inputs live in
[examples/](examples/) and are entirely fictional — do not copy vault
content there.

## Typical workflow

1. Prepare a clean vault copy; note its commit.
2. Start the MCP server under test (vault-mcp with
   `--transport http`, or the sage-wiki `serve` process).
3. Run `retrieval` for the search metrics, `latency` per tool.
4. Run `agent` twice with the same flags for the end-to-end answers.
5. Run write tasks and their post-checks.
6. Aggregate with `report`; do the scoring and the write-up from the
   raw files.
