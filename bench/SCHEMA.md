# Bench file formats

All input and result files are UTF-8. Inputs are JSON Lines (one JSON
object per line); blank lines are ignored.

## questions.jsonl

One question per line:

```json
{
  "id": "A01",
  "type": "A",
  "question": "Question text",
  "query": "keyword query for the search tool (optional; falls back to question)",
  "answer": "The gold (standard) answer",
  "gold_pages": ["wiki/concepts/example.md"],
  "related_pages": ["wiki/sources/example-source.md"],
  "notes": "Why these pages; for type B, the original wording; for type E, the grep terms that prove the content does not exist"
}
```

- `type` is one of A (keyword lookup), B (rephrased concept), C
  (multi-hop, 2–3 pages), D (structure navigation), E (nonexistent
  content).
- `gold_pages` are vault-relative note paths. Empty for type E.
- Types A–D need at least one gold page; type C needs at least two.
- `related_pages` (optional) are other pages that genuinely answer the
  question, for example pages that contain the same keyword. They
  count as hits for MRR but do not enter the Recall denominator, so a
  related page ranked first does not penalize MRR.

### Scoring rules (identical for every target)

- **index.md is dropped** from the result list before Recall/MRR are
  computed: it lists every page name and would otherwise match every
  keyword. It is kept only when a question uses it as a gold page.
- **Recall@5** = fraction of `gold_pages` in the first 5 results.
- **MRR@10** = mean reciprocal rank of each gold page in the first 10
  results, where a hit on any `related_pages` entry counts as a hit at
  that rank.
- The retrieval tool is run twice: once with `-search-text question`
  (the natural question text, what an agent often sends) and once with
  `-search-text query` (the keyword query). Both are reported for both
  targets; the question text is what makes the "long sentence returns
  nothing" behaviour visible.

## write_tasks.jsonl

One task per line:

```json
{
  "id": "W01",
  "task": "Task description for the agent",
  "expected_changes": ["raw/2026-01-01-note.md"],
  "checks": ["lint", "git-diff-scope", "rules"],
  "notes": "Context for the reviewer"
}
```

`checks` names the post-checks the harness must run after the task
(lint script, git diff review, vault rule compliance). These run
outside the agent; record their outcome next to the results.

## retrieval.json

Written by `cmd/retrieval`. Top level:

```json
{
  "target": "vault",
  "search_tool": "search_notes",
  "limit": 10,
  "generated_at": "...",
  "recall_mrr_by_type": { "A": {"n": 10, "mean": 0.8, ...} },
  "questions": [
    {
      "id": "A01",
      "type": "A",
      "question": "...",
      "gold_pages": ["..."],
      "results": [{"path": "...", "score": 12.3}],
      "hit_ranks": {"wiki/...": 1},
      "first_hit_rank": 1,
      "duration_ms": 12.4
    }
  ]
}
```

`retrieval.csv` has one row per question (`id,type,search_text,
first_hit_rank,recall_at_5,mrr_at_10,num_results`) plus `SUMMARY` rows
per type. `search_text` records the exact string sent to the search
tool, so the question-mode and query-mode runs are distinguishable.

## agent.jsonl

One line per question or write task, written by `cmd/agent`:

```json
{
  "run": "v-run1",
  "phase": "agent",
  "id": "A01",
  "type": "A",
  "question": "...",
  "answer": "final answer text",
  "citations": ["wiki/..."],
  "tool_calls": 4,
  "prompt_tokens": 9000,
  "completion_tokens": 400,
  "cost_usd": 0.0009,
  "duration_ms": 8000,
  "truncated": false,
  "finish_reason": "stop",
  "error": ""
}
```

`citations` are note paths harvested from the final answer (wikilinks
or `.md` code spans). They are candidates, not truth: citation
correctness is judged by the reviewer against `gold_pages`.

## cost.jsonl

One line per OpenRouter generation, written by `cmd/agent`:

```json
{
  "phase": "agent",
  "generation_id": "gen-...",
  "model": "deepseek/deepseek-v4-flash",
  "prompt_tokens": 9000,
  "completion_tokens": 400,
  "cached_tokens": 8000,
  "cost_usd": 0.0009,
  "time": "2026-10-17T01:00:00Z"
}
```

Never contains the API key or request bodies. Phase names separate the
budget buckets (e.g. compile, agent, sage-query).

## latency.json

Written by `cmd/latency`: tool name, args, cold/warm summaries
(`n`, `mean_ms`, `p50_ms`, `p95_ms`, `min_ms`, `max_ms`) and the raw
samples. `latency.csv` has `mode,ms` rows.

## Summary JSON

`cmd/report` aggregates one results directory into a summary JSON.
