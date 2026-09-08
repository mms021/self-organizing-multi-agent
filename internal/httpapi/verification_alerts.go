package httpapi

import (
	"context"
	"fmt"
	"time"
)

// RunVerificationAlerts scans durable exhausted jobs, even with no agents online.
func (s *Server) RunVerificationAlerts(ctx context.Context) {
	if _, ok := s.OperatorNotifier.(TelegramMessenger); !ok || s.TelegramUserID == "" {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.sendVerificationAlerts(ctx); err != nil && ctx.Err() == nil && s.Logger != nil {
			// Do not log the Telegram HTTP error: it may contain the bot token.
			s.Logger.Print("verification alert delivery failed; will retry")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) sendVerificationAlerts(ctx context.Context) error {
	messenger, ok := s.OperatorNotifier.(TelegramMessenger)
	if !ok {
		return nil
	}
	for i := 0; i < 10; i++ {
		taskID, token, err := s.Tasks.ClaimVerificationAlert(ctx)
		if err != nil {
			return err
		}
		if taskID == "" {
			return nil
		}
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = messenger.Send(sendCtx, fmt.Sprintf("Verification needs attention: %s\nAutomatic attempts exhausted. The task remains SUBMITTED.\nRetry verification only:\n/retry %s\nThe creator agent must be running.", taskID, taskID))
		cancel()
		if err != nil {
			return err
		} // lease expires; retry survives restart
		if err := s.Tasks.MarkVerificationAlertSent(ctx, taskID, token); err != nil {
			return err
		}
	}
	return nil
}
