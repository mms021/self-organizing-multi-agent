// Command mcp runs the agent-scoped MCP stdio adapter. Stdout is protocol only.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"aichatdeck/internal/agent"
	"aichatdeck/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	path := flag.String("state", "", "existing owner-only agent state file (required)")
	flag.Parse()
	if *path == "" {
		log.Fatal("-state is required; create an agent credential before connecting MCP")
	}
	state, ok, err := agent.LoadState(*path)
	if err != nil || !ok {
		log.Fatal("cannot load valid owner-only agent state")
	}
	server, err := mcpbridge.New(state.BaseURL, state.AgentID, state.Token)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal("MCP stdio session failed")
	}
}
