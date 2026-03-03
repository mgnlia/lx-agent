package binding

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/lib/pq"
)

type Store struct {
	db *sql.DB
}

func New(databaseURL string) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema tx: %w", err)
	}
	defer tx.Rollback()

	// Avoid concurrent CREATE TABLE races during rolling deploy/startup.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(781234567890123)`); err != nil {
		return fmt.Errorf("acquire schema lock: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS telegram_bindings (
  canvas_api_key TEXT PRIMARY KEY,
  chat_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`)
	if err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS telegram_chat_settings (
  chat_id TEXT PRIMARY KEY,
  lang TEXT NOT NULL DEFAULT 'ko',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`)
	if err != nil {
		return fmt.Errorf("ensure chat settings schema: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema tx: %w", err)
	}
	return nil
}

func (s *Store) Upsert(ctx context.Context, canvasAPIKey, chatID string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO telegram_bindings (canvas_api_key, chat_id)
VALUES ($1, $2)
ON CONFLICT (canvas_api_key)
DO UPDATE SET
  chat_id = EXCLUDED.chat_id,
  updated_at = NOW()
`, canvasAPIKey, chatID)
	if err != nil {
		return fmt.Errorf("upsert binding: %w", err)
	}
	return nil
}

func (s *Store) LookupChatID(ctx context.Context, canvasAPIKey string) (string, error) {
	var chatID string
	err := s.db.QueryRowContext(ctx, `
SELECT chat_id
FROM telegram_bindings
WHERE canvas_api_key = $1
`, canvasAPIKey).Scan(&chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup chat id: %w", err)
	}
	return chatID, nil
}

func (s *Store) SetChatLanguage(ctx context.Context, chatID, lang string) error {
	if lang != "ko" && lang != "en" {
		return fmt.Errorf("unsupported language: %s", lang)
	}

	_, err := s.db.ExecContext(ctx, `
INSERT INTO telegram_chat_settings (chat_id, lang)
VALUES ($1, $2)
ON CONFLICT (chat_id)
DO UPDATE SET
  lang = EXCLUDED.lang,
  updated_at = NOW()
`, chatID, lang)
	if err != nil {
		return fmt.Errorf("set chat language: %w", err)
	}
	return nil
}

func (s *Store) GetChatLanguage(ctx context.Context, chatID string) (string, error) {
	var lang string
	err := s.db.QueryRowContext(ctx, `
SELECT lang
FROM telegram_chat_settings
WHERE chat_id = $1
`, chatID).Scan(&lang)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get chat language: %w", err)
	}
	return lang, nil
}
