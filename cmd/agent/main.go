// Command agent runs one platform agent: it registers, finds open tasks it
// can do, claims them, works them through its brain, and verifies results for
// tasks it created itself.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"aichatdeck/internal/agent"
	"aichatdeck/internal/model"
)

func main() {
	var (
		base     = flag.String("base", "http://localhost:8080", "platform base URL")
		name     = flag.String("name", "agent", "agent name, reported as operator")
		capsFlag = flag.String("caps", "", "comma-separated capabilities")
		task     = flag.String("task", "", "if set, create this task at startup and verify its result")
		brain    = flag.String("brain", "auto", "brain: auto|claude|echo")
		once     = flag.Bool("once", false, "run a single iteration and exit")
		wait     = flag.Int("wait", 10, "inbox long-poll seconds per idle iteration")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	// An agent with no declared capabilities cannot be matched to any task,
	// so the platform rejects the registration — fail here with something
	// actionable instead of surfacing a 400 from the server.
	if *capsFlag == "" {
		logger.Fatal("-caps is required: declare at least one capability, e.g. -caps research,summarizing")
	}
	caps := strings.Split(*capsFlag, ",")

	a := &agent.Agent{
		Client:   agent.NewClient(*base),
		Brain:    pickBrain(*brain, logger),
		Name:     *name,
		Caps:     caps,
		Log:      logger,
		PollWait: *wait,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Bootstrap(ctx); err != nil {
		logger.Fatalf("bootstrap: %v", err)
	}

	if *task != "" {
		created, err := a.Client.CreateTask(ctx, model.CreateTaskRequest{Objective: *task})
		if err != nil {
			logger.Fatalf("create task: %v", err)
		}
		logger.Printf("[%s] created task %s: %s", *name, created.TaskID, created.Objective)
	}

	if *once {
		if _, err := a.RunOnce(ctx, *wait); err != nil {
			logger.Fatalf("run: %v", err)
		}
		return
	}
	if err := a.Run(ctx); err != nil {
		logger.Fatalf("run: %v", err)
	}
}

// pickBrain resolves "auto" against the environment: a real model when
// credentials are present, the offline stub otherwise. Either way it says
// which one it chose — silently degrading to a stub would make a broken
// agent look like a working one.
func pickBrain(choice string, logger *log.Logger) agent.Brain {
	switch choice {
	case "echo":
		logger.Print("brain: echo (deterministic stub)")
		return agent.EchoBrain{}
	case "claude":
		logger.Print("brain: claude")
		return agent.NewClaudeBrain(os.Getenv("ANTHROPIC_API_KEY"))
	default:
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			logger.Print("brain: claude (ANTHROPIC_API_KEY found)")
			return agent.NewClaudeBrain(os.Getenv("ANTHROPIC_API_KEY"))
		}
		logger.Print("brain: echo (no ANTHROPIC_API_KEY; pass -brain claude to force)")
		return agent.EchoBrain{}
	}
}
