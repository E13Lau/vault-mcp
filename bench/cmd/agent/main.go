// Command agent runs the end-to-end benchmark: a simple agent loop
// that answers each question using only the MCP tools of the system
// under test, with the answering model served over the OpenRouter
// OpenAI-compatible API. It also runs the write tasks.
//
// The agent has no shell, filesystem or network access of its own:
// every action it takes goes through the MCP server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/E13Lau/vault-mcp/bench/internal/ledger"
	"github.com/E13Lau/vault-mcp/bench/internal/mcpclient"
	"github.com/E13Lau/vault-mcp/bench/internal/openrouter"
	"github.com/E13Lau/vault-mcp/bench/internal/questions"
)

type flags struct {
	questionsFile string
	tasksFile     string
	outDir        string
	target        string
	mcpURL        string
	stdioCmd      string
	model         string
	baseURL       string
	apiKeyEnv     string
	instructions  string // "init" = MCP initialize instructions; otherwise a file path
	systemPrompt  string // file path of the generic system prompt
	temperature   float64
	seed          int64
	reasoning     string
	providerSort  string
	maxToolCalls  int
	maxInputTok   int64
	runs          int
	runLabel      string
	phase         string
	budgetUSD     float64
	timeout       time.Duration
}

func main() {
	var f flags
	flag.StringVar(&f.questionsFile, "questions", "", "questions.jsonl file (all questions run unless -types is given)")
	flag.StringVar(&f.tasksFile, "tasks", "", "write_tasks.jsonl file (optional; when given, write tasks run instead of questions)")
	flag.StringVar(&f.outDir, "out", "", "output directory")
	flag.StringVar(&f.target, "target", "vault", "system under test: vault or sage")
	flag.StringVar(&f.mcpURL, "mcp-url", "", "Streamable HTTP endpoint URL")
	flag.StringVar(&f.stdioCmd, "stdio-cmd", "", "server command for the stdio transport")
	flag.StringVar(&f.model, "model", "deepseek/deepseek-v4-flash", "answering model (OpenRouter id)")
	flag.StringVar(&f.baseURL, "base-url", "https://openrouter.ai/api/v1", "OpenRouter-compatible base URL")
	flag.StringVar(&f.apiKeyEnv, "api-key-env", "OPENROUTER_API_KEY", "environment variable holding the API key")
	flag.StringVar(&f.instructions, "instructions", "init", `usage instructions: "init" (MCP initialize response) or a file path`)
	flag.StringVar(&f.systemPrompt, "system-prompt", "", "generic system prompt file; default is built in")
	flag.Float64Var(&f.temperature, "temperature", 0, "model temperature")
	flag.Int64Var(&f.seed, "seed", 42, "model seed")
	flag.StringVar(&f.reasoning, "reasoning", "none", `reasoning effort ("none", "minimal", ...); "" = do not send`)
	flag.StringVar(&f.providerSort, "provider-sort", "throughput", `OpenRouter provider.sort (e.g. "throughput"); "" = API default`)
	flag.IntVar(&f.maxToolCalls, "max-tool-calls", 15, "tool call limit per question")
	flag.Int64Var(&f.maxInputTok, "max-input-tokens", 60000, "input token budget per question")
	flag.IntVar(&f.runs, "runs", 1, "number of identical runs (1 or 2)")
	flag.StringVar(&f.runLabel, "run-label", "", "label for this run set (e.g. v-run1)")
	flag.StringVar(&f.phase, "phase", "agent", "cost ledger phase name")
	flag.Float64Var(&f.budgetUSD, "budget", 0, "phase cost cap in USD; 0 = unlimited")
	flag.DurationVar(&f.timeout, "timeout", 180*time.Second, "per-call timeout")
	flag.Parse()
	if f.outDir == "" {
		fatal("usage: agent -questions <file> -out <dir> (-mcp-url <url> | -stdio-cmd <cmd>) [flags]")
	}
	if (f.mcpURL == "") == (f.stdioCmd == "") {
		fatal("exactly one of -mcp-url or -stdio-cmd is required")
	}
	if err := run(f); err != nil {
		fatal("%v", err)
	}
}

// toolTrace is one tool call, kept small enough for the results file.
type toolTrace struct {
	Name string `json:"name"`
	Args string `json:"args"`
}

// questionRecord is one line of agent.jsonl.
type questionRecord struct {
	Run             string      `json:"run"`
	Phase           string      `json:"phase"`
	ID              string      `json:"id"`
	Type            string      `json:"type"`
	Question        string      `json:"question"`
	Answer          string      `json:"answer"`
	Citations       []string    `json:"citations"`
	ToolCalls       int         `json:"tool_calls"`
	ToolTrace       []toolTrace `json:"tool_trace,omitempty"`
	ClaudeMDTouched bool        `json:"claude_md_touched,omitempty"`
	PromptTok       int64       `json:"prompt_tokens"`
	ComplTok        int64       `json:"completion_tokens"`
	CostUSD         float64     `json:"cost_usd"`
	DurationMs      float64     `json:"duration_ms"`
	Truncated       bool        `json:"truncated"`
	Finish          string      `json:"finish_reason"`
	Error           string      `json:"error,omitempty"`
}

const defaultSystemPrompt = `You answer questions using only the tools you are given. Follow the usage instructions you received for this knowledge base.

Rules:
- Always call at least one knowledge-base tool before answering. Never claim to have searched unless you actually called a tool.
- Prefer the search tool to locate notes, then read the ones that look relevant.
- Answer in the language of the question.
- Answer only from what the notes say. If the notes do not contain the answer, say that you could not find it in the knowledge base; never invent facts.
- After your answer, add a final section "Citations:" that lists the exact note paths that support your answer, one per line. List only notes you actually used.`

func run(f flags) error {
	if err := os.MkdirAll(f.outDir, 0o755); err != nil {
		return err
	}
	var qs []questions.Question
	var err error
	if f.questionsFile != "" {
		qs, err = questions.LoadQuestions(f.questionsFile)
		if err != nil {
			return err
		}
	}
	var tasks []questions.WriteTask
	if f.tasksFile != "" {
		tasks, err = questions.LoadWriteTasks(f.tasksFile)
		if err != nil {
			return err
		}
	}
	key, err := openrouter.KeyFromEnv(f.apiKeyEnv)
	if err != nil {
		return err
	}
	ledgerPath := filepath.Join(f.outDir, "cost.jsonl")
	led, err := ledger.New(ledgerPath)
	if err != nil {
		return err
	}
	orc := openrouter.NewClient(f.model, f.baseURL, key)
	orc.SetParams(f.temperature, f.seed, f.reasoning)
	orc.SetProviderSort(f.providerSort)
	orc.SetTimeout(120 * time.Second)
	resultsPath := filepath.Join(f.outDir, "agent.jsonl")
	results, err := os.OpenFile(resultsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer results.Close()
	enc := json.NewEncoder(results)
	enc.SetEscapeHTML(false)

	client, err := dial(f)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx := context.Background()
	init, err := client.Initialize(ctx)
	if err != nil {
		return err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return err
	}
	instructions, err := loadInstructions(f, init)
	if err != nil {
		return err
	}
	chatTools := toChatTools(tools)
	system := defaultSystemPrompt
	if f.systemPrompt != "" {
		b, err := os.ReadFile(f.systemPrompt)
		if err != nil {
			return err
		}
		system = string(b)
	}
	if instructions != "" {
		system += "\n\n--- Knowledge base usage instructions ---\n" + instructions
	}
	fmt.Printf("server=%s/%s tools=%d instructions=%d chars system=%d chars\n",
		init.ServerInfo.Name, init.ServerInfo.Version, len(tools), len(instructions), len(system))

	runErr := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	loop := &agentLoop{
		orc:    orc,
		client: client,
		tools:  chatTools,
		system: system,
		flags:  f,
		ledger: led,
		ctx:    ctx,
	}
	for runIdx := 1; runIdx <= f.runs; runIdx++ {
		if err := checkBudget(ledgerPath, f.phase, f.budgetUSD); err != nil {
			runErr("run %d skipped: %v", runIdx, err)
			break
		}
		label := f.runLabel
		if f.runs > 1 {
			label = fmt.Sprintf("%s-run%d", f.runLabel, runIdx)
		}
		for _, q := range qs {
			rec := loop.answer(q, label)
			if err := enc.Encode(rec); err != nil {
				return err
			}
			fmt.Printf("%s %s calls=%d in=%d out=%d cost=%.4f dur=%.1fms%s\n",
				label, q.ID, rec.ToolCalls, rec.PromptTok, rec.ComplTok, rec.CostUSD, rec.DurationMs, truncOrEmpty(rec))
		}
		for _, t := range tasks {
			rec := loop.answer(questions.Question{ID: t.ID, Type: "W", Question: t.Task}, label)
			if err := enc.Encode(rec); err != nil {
				return err
			}
			fmt.Printf("%s %s calls=%d in=%d out=%d cost=%.4f dur=%.1fms%s\n",
				label, t.ID, rec.ToolCalls, rec.PromptTok, rec.ComplTok, rec.CostUSD, rec.DurationMs, truncOrEmpty(rec))
		}
	}
	total, _ := ledger.Total(ledgerPath)
	fmt.Printf("ledger total for all phases in %s: %.4f USD\n", ledgerPath, total)
	return nil
}

// agentLoop carries the state shared by every question in a run.
type agentLoop struct {
	orc    *openrouter.Client
	client mcpclient.Client
	tools  []openrouter.ChatTool
	system string
	flags  flags
	ledger *ledger.File
	ctx    context.Context
}

// answer runs the tool-use loop for one question.
func (l *agentLoop) answer(q questions.Question, label string) (rec questionRecord) {
	f := l.flags
	rec = questionRecord{
		Run:      label,
		Phase:    f.phase,
		ID:       q.ID,
		Type:     q.Type,
		Question: q.Question,
	}
	start := time.Now()
	// Named return: the deferred write reaches the value the caller sees.
	defer func() { rec.DurationMs = float64(time.Since(start).Microseconds()) / 1000 }()
	messages := []openrouter.ChatMessage{
		{Role: "system", Content: l.system},
		{Role: "user", Content: q.Question},
	}
	var answer strings.Builder
	for call := 0; call < f.maxToolCalls; call++ {
		comp, err := l.orc.Complete(l.ctx, messages, l.tools)
		if err != nil {
			// One retry on a transport error (timeout, reset). A
			// deterministic API error fails the same way twice.
			comp, err = l.orc.Complete(l.ctx, messages, l.tools)
		}
		if err != nil {
			rec.Error = err.Error()
			break
		}
		rec.PromptTok += comp.Usage.PromptTokens
		rec.ComplTok += comp.Usage.CompletionTokens
		rec.CostUSD += l.orc.Cost(comp)
		rec.Finish = comp.Finish
		if l.ledger != nil {
			_ = l.ledger.Add(ledger.Entry{
				Phase:            f.phase,
				GenerationID:     comp.ID,
				Model:            comp.Model,
				PromptTokens:     comp.Usage.PromptTokens,
				CompletionTokens: comp.Usage.CompletionTokens,
				CachedTokens:     comp.Usage.CachedTokens,
				CostUSD:          l.orc.Cost(comp),
				Time:             time.Now(),
			})
		}
		if len(comp.ToolCalls) == 0 {
			answer.WriteString(comp.Message.Content)
			break
		}
		// A truncated input budget ends the loop after this turn.
		if rec.PromptTok > f.maxInputTok {
			rec.Truncated = true
			break
		}
		messages = append(messages, openrouter.AssistantToolCallMessage(comp.ToolCalls))
		for _, tc := range comp.ToolCalls {
			rec.ToolCalls++
			args, err := parseArgs(tc.Arguments)
			if err != nil {
				messages = append(messages, openrouter.ToolResultMessage(tc.ID,
					"tool arguments are not valid JSON: "+err.Error()))
				continue
			}
			cr, err := l.client.CallTool(l.ctx, tc.Name, args)
			content := ""
			if err != nil {
				content = "tool error: " + err.Error()
			} else if cr.IsError {
				content = "tool returned an error: " + cr.Text()
			} else {
				content = cr.Text()
			}
			argJSON, _ := json.Marshal(args)
			if len(argJSON) > 300 {
				argJSON = argJSON[:300]
			}
			rec.ToolTrace = append(rec.ToolTrace, toolTrace{Name: tc.Name, Args: string(argJSON)})
			if strings.Contains(string(argJSON), "CLAUDE.md") || strings.Contains(content, "Vault guide (CLAUDE.md)") {
				rec.ClaudeMDTouched = true
			}
			messages = append(messages, openrouter.ToolResultMessage(tc.ID, content))
		}
	}
	rec.Answer = answer.String()
	rec.Citations = extractCitations(rec.Answer)
	return rec
}

func parseArgs(s string) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// extractCitations collects the pages the answer cites: note paths that
// end in .md, plus wikilink names like [[页面]] or [[页面|别名]].
// Names are kept as written; consumers match them against gold pages
// by basename.
func extractCitations(answer string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	// Wikilinks.
	for rest := answer; ; {
		i := strings.Index(rest, "[[")
		if i < 0 {
			break
		}
		rest = rest[i+2:]
		j := strings.Index(rest, "]]")
		if j < 0 {
			break
		}
		link := rest[:j]
		if k := strings.IndexAny(link, "|#"); k >= 0 {
			link = link[:k]
		}
		add(link)
		rest = rest[j+2:]
	}
	// Paths in code spans or plain text.
	for _, line := range strings.Split(answer, "\n") {
		for _, tok := range strings.FieldsFunc(line, func(r rune) bool {
			return r == '`' || r == '[' || r == ']' || r == '(' || r == ')' || r == '|' || r == ',' || r == ' ' || r == '：' || r == ':'
		}) {
			tok = strings.TrimSpace(tok)
			if strings.HasSuffix(tok, ".md") && strings.Contains(tok, "/") {
				add(tok)
			}
		}
	}
	return out
}

func toChatTools(tools []mcpclient.Tool) []openrouter.ChatTool {
	out := make([]openrouter.ChatTool, 0, len(tools))
	for _, t := range tools {
		var ct openrouter.ChatTool
		ct.Type = "function"
		ct.Function.Name = t.Name
		ct.Function.Description = t.Description
		ct.Function.Parameters = t.InputSchema
		out = append(out, ct)
	}
	return out
}

func loadInstructions(f flags, init *mcpclient.InitResult) (string, error) {
	if f.instructions == "init" {
		return init.Instructions, nil
	}
	b, err := os.ReadFile(f.instructions)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func checkBudget(ledgerPath, phase string, capUSD float64) error {
	if capUSD <= 0 {
		return nil
	}
	total, err := ledger.PhaseTotal(ledgerPath, phase)
	if err != nil {
		return err
	}
	if total >= capUSD {
		return fmt.Errorf("phase %s cost %.4f USD already reached its %.2f USD cap", phase, total, capUSD)
	}
	return nil
}

func dial(f flags) (mcpclient.Client, error) {
	if f.stdioCmd != "" {
		parts, err := shellSplit(f.stdioCmd)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("empty -stdio-cmd")
		}
		logs, err := os.Create(filepath.Join(f.outDir, "mcp-server.log"))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "server stderr: %s\n", filepath.Join(f.outDir, "mcp-server.log"))
		return mcpclient.NewStdioClient(parts[0], parts[1:], logs), nil
	}
	token := os.Getenv("MCP_TOKEN")
	return mcpclient.NewHTTPClient(f.mcpURL, token, f.timeout), nil
}

// shellSplit splits a command line on whitespace, honouring double
// quotes (same behaviour as the retrieval tool).
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur []rune
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = nil
			}
		default:
			cur = append(cur, r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in -stdio-cmd")
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out, nil
}

func truncOrEmpty(rec questionRecord) string {
	if rec.Error == "" {
		return ""
	}
	if len(rec.Error) > 80 {
		rec.Error = rec.Error[:80] + "..."
	}
	return " err=" + rec.Error
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agent: "+format+"\n", args...)
	os.Exit(1)
}
