package notifier

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

type TelegramNotifier struct {
	botToken string
	chatID   string
}

func NewTelegram(botToken, chatID string) *TelegramNotifier {
	return &TelegramNotifier{botToken: botToken, chatID: chatID}
}

func (t *TelegramNotifier) Send(ctx context.Context, message string) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	params := url.Values{
		"chat_id":    {t.chatID},
		"text":       {message},
		"parse_mode": {"Markdown"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram error: %d", resp.StatusCode)
	}
	return nil
}
