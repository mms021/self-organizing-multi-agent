package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"aichatdeck/internal/model"
)

// Telegram forwards operator requests. Its credentials stay in server config;
// agents only ever see a success or failure response from the platform.
type Telegram struct {
	Token, ChatID string
	HTTP          *http.Client
}

type response struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
}

func (t *Telegram) Notify(ctx context.Context, agentID string, req model.OperatorRequest) (int64, error) {
	return t.Send(ctx, fmt.Sprintf("AI Chat Deck — %s\nFrom: %s\n%s\n\n%s", req.Kind, agentID, req.Title, req.Details))
}

func (t *Telegram) Send(ctx context.Context, text string) (int64, error) {
	client := t.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	body, _ := json.Marshal(map[string]string{
		"chat_id": t.ChatID,
		"text":    text,
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("telegram returned %s", resp.Status)
	}
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if !out.OK || out.Result.MessageID == 0 {
		return 0, fmt.Errorf("telegram did not return a message id")
	}
	return out.Result.MessageID, nil
}
