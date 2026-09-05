package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"aichatdeck/internal/model"
)

// Brain is the thinking half of an agent. The loop (client.go, agent.go) is
// deterministic plumbing; everything judgement-shaped goes through here, so
// the whole platform can be exercised offline with EchoBrain and with a real
// model in production.
type Brain interface {
	// Work produces a result summary for a task objective. prior is whatever
	// relevant shared memory the agent found before starting (RFC-1300) —
	// reusing it is the point of having a knowledge base at all.
	Work(ctx context.Context, task model.Task, prior []model.Knowledge) (summary string, status string, err error)
	// Verify judges someone else's result against the task it claims to solve.
	Verify(ctx context.Context, task model.Task, resultSummary string) (verdict, rationale string, err error)
}

// EchoBrain is a deterministic stub: no API key, no network, no judgement.
// It exists so the full agent loop can be tested and demoed offline.
type EchoBrain struct{}

func (EchoBrain) Work(_ context.Context, task model.Task, prior []model.Knowledge) (string, string, error) {
	summary := "echo: " + task.Objective
	if len(prior) > 0 {
		summary += fmt.Sprintf(" (reused %d prior knowledge entries)", len(prior))
	}
	return summary, "success", nil
}

func (EchoBrain) Verify(_ context.Context, task model.Task, resultSummary string) (string, string, error) {
	// Only checks that the result actually references the objective — enough
	// to make the loop meaningful in tests without pretending to be judgement.
	if strings.Contains(resultSummary, task.Objective) {
		return "verified", "result references the stated objective", nil
	}
	return "inconclusive", "echo brain cannot judge this result", nil
}

// ClaudeBrain calls the Anthropic API, with prompt.md as its system prompt so
// the model knows the platform's rules before it is asked to act inside them.
type ClaudeBrain struct {
	client anthropic.Client
	model  string
}

func NewClaudeBrain(apiKey string) *ClaudeBrain {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &ClaudeBrain{
		client: anthropic.NewClient(opts...),
		model:  "claude-opus-5",
	}
}

func (b *ClaudeBrain) ask(ctx context.Context, prompt string) (string, error) {
	resp, err := b.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(b.model),
		MaxTokens: 16000,
		System: []anthropic.TextBlockParam{{
			Text:         SystemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(text.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

func (b *ClaudeBrain) Work(ctx context.Context, task model.Task, prior []model.Knowledge) (string, string, error) {
	prompt := fmt.Sprintf(`Ты взял задачу на платформе. Выполни её и опиши результат.

objective: %s
success_criteria: %v
constraints: %v

Ответь кратким summary результата — тем, что другой агент сможет проверить.
Первая строка ответа: SUCCESS, PARTIAL или FAILURE. Дальше — summary.`,
		task.Objective, task.SuccessCriteria, task.Constraints)

	if known := formatKnowledge(prior); known != "" {
		prompt += "\n\nЧто уже известно из общей памяти (используй, не переоткрывай):\n" + known
	}

	out, err := b.ask(ctx, prompt)
	if err != nil {
		return "", "", err
	}

	status, summary := splitFirstLine(out)
	switch strings.ToUpper(status) {
	case "SUCCESS":
		return summary, "success", nil
	case "PARTIAL":
		return summary, "partial", nil
	case "FAILURE":
		return summary, "failure", nil
	default:
		// Model ignored the format: keep the whole answer, report honestly
		// that we can't tell how complete it is.
		return out, "partial", nil
	}
}

func (b *ClaudeBrain) Verify(ctx context.Context, task model.Task, resultSummary string) (string, string, error) {
	prompt := fmt.Sprintf(`Другой агент выполнил задачу, которую создал ты. Проверь результат.

objective: %s
success_criteria: %v

результат агента:
%s

Первая строка ответа: VERIFIED, REJECTED или INCONCLUSIVE. Дальше — обоснование в одну-две фразы.
Ставь VERIFIED только если результат действительно отвечает objective. Если проверить нечем — INCONCLUSIVE, а не VERIFIED.`,
		task.Objective, task.SuccessCriteria, resultSummary)

	out, err := b.ask(ctx, prompt)
	if err != nil {
		return "", "", err
	}

	verdict, rationale := splitFirstLine(out)
	switch strings.ToUpper(verdict) {
	case "VERIFIED":
		return "verified", rationale, nil
	case "REJECTED":
		return "rejected", rationale, nil
	default:
		return "inconclusive", rationale, nil
	}
}

func splitFirstLine(s string) (first, rest string) {
	parts := strings.SplitN(strings.TrimSpace(s), "\n", 2)
	first = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}
	return first, rest
}

// formatKnowledge renders shared-memory entries for a prompt, marking each
// one's status so the model can tell a verified fact from an unproven
// hypothesis (RFC-1300 §5).
func formatKnowledge(entries []model.Knowledge) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, k := range entries {
		summary, _ := k.Content["summary"].(string)
		if summary == "" {
			summary = fmt.Sprintf("%v", k.Content)
		}
		fmt.Fprintf(&sb, "- [%s/%s] %s\n", k.Category, k.Status, summary)
	}
	return sb.String()
}
