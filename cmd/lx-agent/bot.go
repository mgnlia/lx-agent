package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mgnlia/lx-agent/internal/binding"
	"github.com/mgnlia/lx-agent/internal/canvas"
	"github.com/mgnlia/lx-agent/internal/monitor"
)

type telegramGetUpdatesResponse struct {
	OK     bool                `json:"ok"`
	Result []telegramBotUpdate `json:"result"`
}

type telegramBotUpdate struct {
	UpdateID      int64                     `json:"update_id"`
	Message       *telegramBotMessage       `json:"message"`
	CallbackQuery *telegramBotCallbackQuery `json:"callback_query"`
}

type telegramBotMessage struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

type telegramBotCallbackQuery struct {
	ID      string              `json:"id"`
	Data    string              `json:"data"`
	Message *telegramBotMessage `json:"message"`
}

type telegramInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

type telegramInlineMarkup struct {
	InlineKeyboard [][]telegramInlineButton `json:"inline_keyboard"`
}

type telegramBotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type botResponse struct {
	Text           string
	Keyboard       *telegramInlineMarkup
	CallbackNotice string
	EditMessage    bool
}

type upcomingAssignment struct {
	CourseName string
	Item       canvas.Assignment
}

func handleBot(cfg config, client *canvas.Client, logger *slog.Logger) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_ = client
	if err := runBotLoop(ctx, cfg, logger); err != nil && !errors.Is(err, context.Canceled) {
		exitErr(err)
	}
}

func handleServe(cfg config, client *canvas.Client, logger *slog.Logger) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !hasCanvasConfig(cfg) {
		if strings.TrimSpace(cfg.Notifier.Telegram.BotToken) == "" {
			exitErr(errors.New("serve requires telegram bot token when canvas is not configured"))
		}
		logger.Warn("canvas is not configured; starting bot only")
		if err := runBotLoop(ctx, cfg, logger); err != nil && !errors.Is(err, context.Canceled) {
			exitErr(err)
		}
		return
	}

	if strings.TrimSpace(cfg.Notifier.Telegram.BotToken) == "" {
		logger.Warn("serve mode without telegram bot token; running monitor only")
		if err := runMonitorService(ctx, cfg, client, logger); err != nil && !errors.Is(err, context.Canceled) {
			exitErr(err)
		}
		return
	}

	errCh := make(chan error, 2)
	go func() { errCh <- runMonitorService(ctx, cfg, client, logger) }()
	go func() { errCh <- runBotLoop(ctx, cfg, logger) }()

	for i := 0; i < 2; i++ {
		err := <-errCh
		if err != nil && !errors.Is(err, context.Canceled) {
			exitErr(err)
		}
	}
}

func runMonitorService(ctx context.Context, cfg config, client *canvas.Client, logger *slog.Logger) error {
	n := buildNotifier(ctx, cfg, logger)

	monitorChatID := ""
	if strings.EqualFold(strings.TrimSpace(cfg.Notifier.Provider), "telegram") {
		cid, err := resolveTelegramChatID(ctx, cfg)
		if err != nil {
			logger.Warn("resolve monitor chat id failed", "err", err)
		} else {
			monitorChatID = cid
		}
	}

	interval, err := time.ParseDuration(cfg.Monitor.PollInterval)
	if err != nil {
		return fmt.Errorf("invalid monitor.poll_interval: %w", err)
	}

	m := monitor.New(client, n, nil, monitor.Config{
		PollInterval:   interval,
		CourseFilter:   cfg.Monitor.Courses,
		SummarizeNew:   cfg.Monitor.SummarizeNew,
		DeadlineAlerts: cfg.Monitor.DeadlineAlerts,
		StatePath:      cfg.Monitor.StatePath,
		DatabaseURL:    cfg.Database.URL,
		ChatID:         monitorChatID,
	}, logger)

	return m.Run(ctx)
}

func runBotLoop(ctx context.Context, cfg config, logger *slog.Logger) error {
	botToken := strings.TrimSpace(cfg.Notifier.Telegram.BotToken)
	if botToken == "" {
		return errors.New("bot mode requires TELEGRAM_BOT_TOKEN")
	}

	allowedChatID, err := resolveTelegramChatID(ctx, cfg)
	if err != nil {
		return fmt.Errorf("resolve bot chat id: %w", err)
	}
	if allowedChatID == "" && hasCanvasConfig(cfg) {
		return errors.New("chat id not bound; run bind-chat first")
	}
	if allowedChatID == "" {
		logger.Warn("chat is not bound; allowing all chats because canvas is not configured")
	}

	logger.Info("telegram command bot started", "chat_id", allowedChatID)
	{
		setupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := syncTelegramCommands(setupCtx, botToken); err != nil {
			logger.Warn("setMyCommands failed", "err", err)
		}
	}

	var offset int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		updates, err := getTelegramUpdates(ctx, botToken, offset, 25)
		if err != nil {
			logger.Warn("getUpdates failed", "err", err)
			time.Sleep(3 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}

			if u.CallbackQuery != nil {
				if u.CallbackQuery.Message == nil {
					_ = answerTelegramCallbackQuery(ctx, botToken, u.CallbackQuery.ID, "")
					continue
				}
				chatID := strconv.FormatInt(u.CallbackQuery.Message.Chat.ID, 10)
				if allowedChatID != "" && chatID != allowedChatID {
					_ = answerTelegramCallbackQuery(ctx, botToken, u.CallbackQuery.ID, msg("ko", "권한이 없습니다.", "Unauthorized."))
					continue
				}

				lang := languageForChat(ctx, cfg, chatID)
				resp := handleTelegramCallback(ctx, cfg, chatID, u.CallbackQuery.Data, lang, logger)
				if strings.TrimSpace(resp.Text) != "" {
					if resp.EditMessage && u.CallbackQuery.Message != nil && u.CallbackQuery.Message.MessageID > 0 {
						if err := editTelegramBotMessage(ctx, botToken, chatID, u.CallbackQuery.Message.MessageID, resp.Text, resp.Keyboard); err != nil {
							logger.Warn("telegram callback edit failed; falling back to send", "err", err)
							if sendErr := sendTelegramBotMessage(ctx, botToken, chatID, resp.Text, resp.Keyboard); sendErr != nil {
								logger.Error("telegram callback reply failed", "err", sendErr)
							}
						}
					} else {
						if err := sendTelegramBotMessage(ctx, botToken, chatID, resp.Text, resp.Keyboard); err != nil {
							logger.Error("telegram callback reply failed", "err", err)
						}
					}
				}
				notice := trimCallbackNotice(resp.CallbackNotice)
				if err := answerTelegramCallbackQuery(ctx, botToken, u.CallbackQuery.ID, notice); err != nil {
					logger.Warn("answerCallbackQuery failed", "err", err)
				}
				continue
			}

			if u.Message == nil {
				continue
			}

			text := strings.TrimSpace(u.Message.Text)
			if text == "" {
				continue
			}

			chatID := strconv.FormatInt(u.Message.Chat.ID, 10)
			if allowedChatID != "" && chatID != allowedChatID {
				_ = sendTelegramBotMessage(ctx, botToken, chatID, "This bot is bound to a different chat. / 이 봇은 다른 채팅에 바인딩되어 있습니다.", nil)
				continue
			}

			lang := languageForChat(ctx, cfg, chatID)
			resp := handleTelegramCommand(ctx, cfg, chatID, text, lang, logger)
			if strings.TrimSpace(resp.Text) == "" {
				continue
			}

			if err := sendTelegramBotMessage(ctx, botToken, chatID, resp.Text, resp.Keyboard); err != nil {
				logger.Error("telegram command reply failed", "err", err)
			}
		}
	}
}

func handleTelegramCommand(ctx context.Context, cfg config, chatID, text, lang string, logger *slog.Logger) botResponse {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return botResponse{}
	}

	cmd := strings.ToLower(fields[0])
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	args := fields[1:]

	if !strings.HasPrefix(cmd, "/") {
		reply, err := requestCodexReply(ctx, chatID, text, lang)
		if err != nil {
			return botResponse{Text: msg(lang, "Codex 응답 실패: ", "Codex reply failed: ") + err.Error()}
		}
		return botResponse{Text: reply}
	}

	switch cmd {
	case "/start":
		return botResponse{
			Text: strings.Join([]string{
				msg(lang, "안녕하세요. 아래 빠른 메뉴에서 시작하세요.", "Welcome. Start from the quick menu below."),
				"",
				botHelpMessage(lang),
			}, "\n"),
			Keyboard: mainMenuKeyboard(lang),
		}
	case "/help":
		return botResponse{Text: botHelpMessage(lang), Keyboard: mainMenuKeyboard(lang)}
	case "/menu":
		return botResponse{Text: botMenuMessage(lang), Keyboard: mainMenuKeyboard(lang)}
	case "/settings":
		return botResponse{Text: msg(lang, "언어를 선택하세요.", "Choose your language."), Keyboard: languageSettingsKeyboard(lang)}
	case "/status":
		return botResponse{Text: botStatusMessage(ctx, cfg, chatID, lang, logger), Keyboard: mainMenuKeyboard(lang)}
	case "/bind":
		if strings.TrimSpace(cfg.Database.URL) == "" {
			return botResponse{Text: msg(lang, "바인딩에는 DATABASE_URL이 필요합니다.", "Binding requires DATABASE_URL.")}
		}
		canvasToken := strings.TrimSpace(cfg.Canvas.Token)
		if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
			canvasToken = strings.TrimSpace(args[0])
		}
		if canvasToken == "" {
			return botResponse{Text: msg(lang, "사용법: /bind <canvas_token>\n(또는 config의 canvas.token 설정)", "Usage: /bind <canvas_token>\n(or set canvas.token in config)")}
		}
		store, err := binding.New(cfg.Database.URL)
		if err != nil {
			return botResponse{Text: msg(lang, "DB 오류: ", "DB error: ") + err.Error()}
		}
		defer store.Close()
		if err := store.EnsureSchema(ctx); err != nil {
			return botResponse{Text: msg(lang, "DB 스키마 오류: ", "DB schema error: ") + err.Error()}
		}
		if err := store.Upsert(ctx, canvasToken, chatID); err != nil {
			return botResponse{Text: msg(lang, "DB 바인딩 오류: ", "DB bind error: ") + err.Error()}
		}
		return botResponse{Text: msg(lang, "현재 채팅을 Canvas 토큰에 바인딩했습니다.", "Bound this chat to the current Canvas token.")}
	case "/listening":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		return botResponse{Text: cmdListening(ctx, cfg, clientForChat, chatID, lang)}
	case "/listen":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		resp, err := cmdListenSelector(ctx, cfg, clientForChat, 0, lang)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		return resp
	case "/unlisten":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		resp, err := cmdUnlistenSelector(ctx, cfg, clientForChat, chatID, 0, lang)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		return resp
	case "/courses":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		return botResponse{Text: cmdCourses(ctx, cfg, clientForChat, chatID, args, lang)}
	case "/assignments":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		if len(args) > 0 {
			if _, err := strconv.Atoi(args[0]); err == nil {
				return botResponse{Text: msg(lang, "course_id 직접 입력은 더 이상 필요하지 않습니다. /assignments 를 눌러 강의를 선택하세요.", "Typing course_id is no longer needed. Use /assignments and pick a course.")}
			}
		}
		resp, err := cmdAssignmentsSelector(ctx, cfg, clientForChat, chatID, 10, 0, lang)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		return resp
	case "/upcoming":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		return botResponse{Text: cmdUpcoming(ctx, cfg, clientForChat, chatID, args, lang)}
	case "/announcements":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		return botResponse{Text: cmdAnnouncements(ctx, cfg, clientForChat, chatID, args, lang)}
	case "/files":
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		if len(args) > 0 {
			if _, err := strconv.Atoi(args[0]); err == nil {
				return botResponse{Text: msg(lang, "course_id 직접 입력은 더 이상 필요하지 않습니다. /files 를 눌러 강의를 선택하세요.", "Typing course_id is no longer needed. Use /files and pick a course.")}
			}
		}
		resp, err := cmdFilesSelector(ctx, cfg, clientForChat, chatID, 10, 0, lang)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		return resp
	case "/chat":
		if len(args) == 0 {
			return botResponse{Text: msg(lang, "사용법: /chat <메시지>", "Usage: /chat <message>")}
		}
		reply, err := requestCodexReply(ctx, chatID, strings.Join(args, " "), lang)
		if err != nil {
			return botResponse{Text: msg(lang, "Codex 응답 실패: ", "Codex reply failed: ") + err.Error()}
		}
		return botResponse{Text: reply}
	default:
		return botResponse{
			Text:     msg(lang, "알 수 없는 명령어입니다. /help 또는 /menu 를 사용하세요.", "Unknown command. Use /help or /menu."),
			Keyboard: mainMenuKeyboard(lang),
		}
	}
}

func handleTelegramCallback(ctx context.Context, cfg config, chatID, data, lang string, logger *slog.Logger) botResponse {
	if strings.HasPrefix(data, "lang:") {
		target := strings.TrimSpace(strings.TrimPrefix(data, "lang:"))
		if target != "ko" && target != "en" {
			return botResponse{Text: msg(lang, "잘못된 언어 선택입니다.", "Invalid language selection.")}
		}
		if err := setChatLanguage(ctx, cfg, chatID, target); err != nil {
			return botResponse{Text: msg(lang, "언어 저장 실패: ", "Failed to save language: ") + err.Error()}
		}
		if target == "en" {
			return botResponse{Text: "Language set to English.", Keyboard: mainMenuKeyboard("en"), CallbackNotice: "Done", EditMessage: true}
		}
		return botResponse{Text: "언어가 한국어로 설정되었습니다.", Keyboard: mainMenuKeyboard("ko"), CallbackNotice: "완료", EditMessage: true}
	}

	if strings.HasPrefix(data, "menu:") {
		action := strings.TrimSpace(strings.TrimPrefix(data, "menu:"))
		switch {
		case action == "home":
			return botResponse{Text: botMenuMessage(lang), Keyboard: mainMenuKeyboard(lang), CallbackNotice: msg(lang, "메뉴", "Menu"), EditMessage: true}
		case action == "settings":
			return botResponse{Text: msg(lang, "언어를 선택하세요.", "Choose your language."), Keyboard: languageSettingsKeyboard(lang), EditMessage: true}
		case action == "status":
			return botResponse{Text: botStatusMessage(ctx, cfg, chatID, lang, logger), Keyboard: mainMenuKeyboard(lang), EditMessage: true}
		case action == "listening":
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			return botResponse{Text: cmdListening(ctx, cfg, clientForChat, chatID, lang), Keyboard: mainMenuKeyboard(lang), EditMessage: true}
		case action == "upcoming":
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			if clientForChat == nil {
				resp := canvasNotConfiguredResponse(lang)
				resp.EditMessage = true
				return resp
			}
			return botResponse{Text: cmdUpcoming(ctx, cfg, clientForChat, chatID, nil, lang), Keyboard: mainMenuKeyboard(lang), EditMessage: true}
		case action == "ann":
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			if clientForChat == nil {
				resp := canvasNotConfiguredResponse(lang)
				resp.EditMessage = true
				return resp
			}
			return botResponse{Text: cmdAnnouncements(ctx, cfg, clientForChat, chatID, nil, lang), Keyboard: mainMenuKeyboard(lang), EditMessage: true}
		case action == "courses":
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			if clientForChat == nil {
				resp := canvasNotConfiguredResponse(lang)
				resp.EditMessage = true
				return resp
			}
			return botResponse{Text: cmdCourses(ctx, cfg, clientForChat, chatID, nil, lang), Keyboard: mainMenuKeyboard(lang), EditMessage: true}
		case strings.HasPrefix(action, "asgsel:"):
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			if clientForChat == nil {
				resp := canvasNotConfiguredResponse(lang)
				resp.EditMessage = true
				return resp
			}
			page := parseSelectorPage(strings.TrimPrefix(action, "asgsel:"))
			resp, err := cmdAssignmentsSelector(ctx, cfg, clientForChat, chatID, 10, page, lang)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			resp.CallbackNotice = msg(lang, "강의 선택", "Pick a course")
			resp.EditMessage = true
			return resp
		case strings.HasPrefix(action, "filsel:"):
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			if clientForChat == nil {
				resp := canvasNotConfiguredResponse(lang)
				resp.EditMessage = true
				return resp
			}
			page := parseSelectorPage(strings.TrimPrefix(action, "filsel:"))
			resp, err := cmdFilesSelector(ctx, cfg, clientForChat, chatID, 10, page, lang)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			resp.CallbackNotice = msg(lang, "강의 선택", "Pick a course")
			resp.EditMessage = true
			return resp
		case strings.HasPrefix(action, "subsel:"):
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			if clientForChat == nil {
				resp := canvasNotConfiguredResponse(lang)
				resp.EditMessage = true
				return resp
			}
			page := parseSelectorPage(strings.TrimPrefix(action, "subsel:"))
			resp, err := cmdListenSelector(ctx, cfg, clientForChat, page, lang)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			resp.CallbackNotice = msg(lang, "강의 선택", "Pick a course")
			resp.EditMessage = true
			return resp
		case strings.HasPrefix(action, "unsubsel:"):
			clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			if clientForChat == nil {
				resp := canvasNotConfiguredResponse(lang)
				resp.EditMessage = true
				return resp
			}
			page := parseSelectorPage(strings.TrimPrefix(action, "unsubsel:"))
			resp, err := cmdUnlistenSelector(ctx, cfg, clientForChat, chatID, page, lang)
			if err != nil {
				return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
			}
			resp.CallbackNotice = msg(lang, "강의 선택", "Pick a course")
			resp.EditMessage = true
			return resp
		case action == "noop":
			return botResponse{}
		default:
			return botResponse{Text: msg(lang, "지원하지 않는 메뉴 동작입니다.", "Unsupported menu action.")}
		}
	}

	if strings.HasPrefix(data, "asg:") {
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		parts := strings.Split(data, ":")
		if len(parts) != 3 {
			return botResponse{Text: msg(lang, "잘못된 선택 데이터입니다.", "Invalid selection payload.")}
		}
		courseID, err := strconv.Atoi(parts[1])
		if err != nil {
			return botResponse{Text: msg(lang, "강의 ID가 잘못되었습니다.", "Invalid course ID.")}
		}
		limit, err := strconv.Atoi(parts[2])
		if err != nil || limit <= 0 {
			limit = 10
		}
		return botResponse{Text: cmdAssignmentsByID(ctx, clientForChat, courseID, limit, lang)}
	}

	if strings.HasPrefix(data, "fil:") {
		clientForChat, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger)
		if err != nil {
			return botResponse{Text: msg(lang, "오류: ", "Error: ") + err.Error()}
		}
		if clientForChat == nil {
			return canvasNotConfiguredResponse(lang)
		}
		parts := strings.Split(data, ":")
		if len(parts) != 3 {
			return botResponse{Text: msg(lang, "잘못된 선택 데이터입니다.", "Invalid selection payload.")}
		}
		courseID, err := strconv.Atoi(parts[1])
		if err != nil {
			return botResponse{Text: msg(lang, "강의 ID가 잘못되었습니다.", "Invalid course ID.")}
		}
		limit, err := strconv.Atoi(parts[2])
		if err != nil || limit <= 0 {
			limit = 10
		}
		return botResponse{Text: cmdFilesByID(ctx, clientForChat, courseID, limit, lang)}
	}

	if strings.HasPrefix(data, "sub:") {
		courseID, err := strconv.Atoi(strings.TrimPrefix(data, "sub:"))
		if err != nil || courseID <= 0 {
			return botResponse{Text: msg(lang, "강의 ID가 잘못되었습니다.", "Invalid course ID.")}
		}
		if err := addChatCourseSubscription(ctx, cfg, chatID, courseID); err != nil {
			return botResponse{Text: msg(lang, "구독 저장 실패: ", "Failed to save subscription: ") + err.Error()}
		}
		return botResponse{CallbackNotice: msg(lang, "구독 저장됨", "Subscribed")}
	}

	if strings.HasPrefix(data, "unsub:") {
		courseID, err := strconv.Atoi(strings.TrimPrefix(data, "unsub:"))
		if err != nil || courseID <= 0 {
			return botResponse{Text: msg(lang, "강의 ID가 잘못되었습니다.", "Invalid course ID.")}
		}
		if err := removeChatCourseSubscription(ctx, cfg, chatID, courseID); err != nil {
			return botResponse{Text: msg(lang, "구독 해제 실패: ", "Failed to remove subscription: ") + err.Error()}
		}
		return botResponse{CallbackNotice: msg(lang, "구독 해제됨", "Unsubscribed")}
	}

	return botResponse{Text: msg(lang, "지원하지 않는 동작입니다.", "Unsupported action.")}
}

func cmdAssignmentsSelector(ctx context.Context, cfg config, client *canvas.Client, chatID string, limit int, page int, lang string) (botResponse, error) {
	courses, err := monitoredCourses(ctx, cfg, client, chatID)
	if err != nil {
		return botResponse{}, err
	}
	if len(courses) == 0 {
		return botResponse{Text: msg(lang, "강의가 없습니다.", "No courses found.")}, nil
	}

	pageItems, page, totalPages := paginateCourses(courses, page, 8)
	kb := &telegramInlineMarkup{}
	for _, c := range pageItems {
		label := c.Name
		if len([]rune(label)) > 48 {
			label = string([]rune(label)[:48]) + "..."
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []telegramInlineButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("asg:%d:%d", c.ID, limit),
		}})
	}
	appendPaginationRow(kb, lang, page, totalPages, "asgsel")
	appendMenuRow(kb, lang)

	return botResponse{Text: selectorPrompt(lang, msg(lang, "과제를 볼 강의를 선택하세요.", "Select a course to view assignments."), page, totalPages), Keyboard: kb}, nil
}

func cmdFilesSelector(ctx context.Context, cfg config, client *canvas.Client, chatID string, limit int, page int, lang string) (botResponse, error) {
	courses, err := monitoredCourses(ctx, cfg, client, chatID)
	if err != nil {
		return botResponse{}, err
	}
	if len(courses) == 0 {
		return botResponse{Text: msg(lang, "강의가 없습니다.", "No courses found.")}, nil
	}

	pageItems, page, totalPages := paginateCourses(courses, page, 8)
	kb := &telegramInlineMarkup{}
	for _, c := range pageItems {
		label := c.Name
		if len([]rune(label)) > 48 {
			label = string([]rune(label)[:48]) + "..."
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []telegramInlineButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("fil:%d:%d", c.ID, limit),
		}})
	}
	appendPaginationRow(kb, lang, page, totalPages, "filsel")
	appendMenuRow(kb, lang)

	return botResponse{Text: selectorPrompt(lang, msg(lang, "파일을 볼 강의를 선택하세요.", "Select a course to view files."), page, totalPages), Keyboard: kb}, nil
}

func cmdCourses(ctx context.Context, cfg config, client *canvas.Client, chatID string, args []string, lang string) string {
	courses, err := monitoredCourses(ctx, cfg, client, chatID)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}
	if len(courses) == 0 {
		return msg(lang, "강의가 없습니다.", "No courses found.")
	}

	keyword := ""
	if len(args) > 0 {
		keyword = strings.ToLower(strings.Join(args, " "))
	}

	var lines []string
	lines = append(lines, msg(lang, "강의 목록:", "Courses:"))
	for _, c := range courses {
		if keyword != "" && !strings.Contains(strings.ToLower(c.Name), keyword) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%d | %s", c.ID, c.Name))
	}

	if len(lines) == 1 {
		return msg(lang, "조건에 맞는 강의가 없습니다.", "No courses matched your filter.")
	}
	return trimForTelegram(strings.Join(lines, "\n"))
}

func cmdAssignments(ctx context.Context, client *canvas.Client, args []string, lang string) string {
	if len(args) == 0 {
		return msg(lang, "사용법: /assignments <course_id> [limit]", "Usage: /assignments <course_id> [limit]")
	}

	courseID, err := strconv.Atoi(args[0])
	if err != nil {
		return msg(lang, "잘못된 course_id 입니다.", "Invalid course_id.")
	}

	limit := 10
	if len(args) > 1 {
		if v, err := strconv.Atoi(args[1]); err == nil && v > 0 {
			limit = v
		}
	}

	return cmdAssignmentsByID(ctx, client, courseID, limit, lang)
}

func cmdAssignmentsByID(ctx context.Context, client *canvas.Client, courseID, limit int, lang string) string {
	if limit > 30 {
		limit = 30
	}
	if limit <= 0 {
		limit = 10
	}

	assignments, err := client.GetAssignments(ctx, courseID)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}
	courseName := strconv.Itoa(courseID)
	if c, err := client.GetCourse(ctx, courseID); err == nil && c != nil && strings.TrimSpace(c.Name) != "" {
		courseName = c.Name
	}
	if len(assignments) == 0 {
		if lang == "en" {
			return fmt.Sprintf("No assignments for %s.", courseName)
		}
		return fmt.Sprintf("%s 강의에 과제가 없습니다.", courseName)
	}

	sort.Slice(assignments, func(i, j int) bool {
		ai, aj := assignments[i].DueAt, assignments[j].DueAt
		if ai == nil && aj == nil {
			return assignments[i].CreatedAt.After(assignments[j].CreatedAt)
		}
		if ai == nil {
			return false
		}
		if aj == nil {
			return true
		}
		return ai.Before(*aj)
	})

	if len(assignments) > limit {
		assignments = assignments[:limit]
	}

	header := fmt.Sprintf("Assignments for %s:", courseName)
	if lang == "ko" {
		header = fmt.Sprintf("%s 과제:", courseName)
	}
	lines := []string{header}
	for _, a := range assignments {
		due := msg(lang, "마감일 없음", "no due date")
		if a.DueAt != nil {
			due = a.DueAt.In(time.FixedZone("KST", 9*3600)).Format("2006-01-02 15:04 KST")
		}
		if lang == "ko" {
			lines = append(lines, fmt.Sprintf("- %s | %s | %.0f점", due, a.Name, a.PointsPossible))
		} else {
			lines = append(lines, fmt.Sprintf("- %s | %s | %.0f pts", due, a.Name, a.PointsPossible))
		}
	}
	return trimForTelegram(strings.Join(lines, "\n"))
}

func cmdUpcoming(ctx context.Context, cfg config, client *canvas.Client, chatID string, args []string, lang string) string {
	days := 14
	limit := 20
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			days = v
		}
	}
	if len(args) > 1 {
		if v, err := strconv.Atoi(args[1]); err == nil && v > 0 {
			limit = v
		}
	}
	if days > 90 {
		days = 90
	}
	if limit > 50 {
		limit = 50
	}

	courses, err := monitoredCourses(ctx, cfg, client, chatID)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}

	now := time.Now()
	until := now.Add(time.Duration(days) * 24 * time.Hour)
	var upcoming []upcomingAssignment

	for _, c := range courses {
		assignments, err := client.GetAssignments(ctx, c.ID)
		if err != nil {
			continue
		}
		for _, a := range assignments {
			if a.DueAt == nil || a.Submitted {
				continue
			}
			if a.DueAt.After(now) && a.DueAt.Before(until) {
				upcoming = append(upcoming, upcomingAssignment{CourseName: c.Name, Item: a})
			}
		}
	}

	sort.Slice(upcoming, func(i, j int) bool {
		return upcoming[i].Item.DueAt.Before(*upcoming[j].Item.DueAt)
	})

	if len(upcoming) == 0 {
		if lang == "ko" {
			return fmt.Sprintf("향후 %d일 이내 마감 과제가 없습니다.", days)
		}
		return fmt.Sprintf("No upcoming assignments in the next %d days.", days)
	}
	if len(upcoming) > limit {
		upcoming = upcoming[:limit]
	}

	header := fmt.Sprintf("Upcoming assignments (%dd):", days)
	if lang == "ko" {
		header = fmt.Sprintf("다가오는 과제 (%d일):", days)
	}
	lines := []string{header}
	for _, item := range upcoming {
		due := item.Item.DueAt.In(time.FixedZone("KST", 9*3600)).Format("2006-01-02 15:04 KST")
		lines = append(lines, fmt.Sprintf("- %s | %s | %s", due, item.CourseName, item.Item.Name))
	}
	return trimForTelegram(strings.Join(lines, "\n"))
}

func cmdAnnouncements(ctx context.Context, cfg config, client *canvas.Client, chatID string, args []string, lang string) string {
	limit := 10
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 30 {
		limit = 30
	}

	courses, err := monitoredCourses(ctx, cfg, client, chatID)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}
	if len(courses) == 0 {
		return msg(lang, "강의가 없습니다.", "No courses found.")
	}

	courseName := make(map[string]string, len(courses))
	ids := make([]int, 0, len(courses))
	for _, c := range courses {
		courseName[fmt.Sprintf("course_%d", c.ID)] = c.Name
		ids = append(ids, c.ID)
	}

	anns, err := client.GetAnnouncements(ctx, ids)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}
	if len(anns) == 0 {
		return msg(lang, "공지가 없습니다.", "No announcements.")
	}

	sort.Slice(anns, func(i, j int) bool {
		return anns[i].PostedAt.After(anns[j].PostedAt)
	})
	if len(anns) > limit {
		anns = anns[:limit]
	}

	header := msg(lang, "최근 공지:", "Recent announcements:")
	lines := []string{header}
	for _, a := range anns {
		name := courseName[a.ContextCode]
		if name == "" {
			name = a.ContextCode
		}
		posted := a.PostedAt.In(time.FixedZone("KST", 9*3600)).Format("2006-01-02")
		lines = append(lines, fmt.Sprintf("- %s | %s | %s", posted, name, a.Title))
	}
	return trimForTelegram(strings.Join(lines, "\n"))
}

func cmdFilesByID(ctx context.Context, client *canvas.Client, courseID, limit int, lang string) string {
	if limit > 30 {
		limit = 30
	}
	if limit <= 0 {
		limit = 10
	}

	files, err := client.GetFiles(ctx, courseID)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}
	courseName := strconv.Itoa(courseID)
	if c, err := client.GetCourse(ctx, courseID); err == nil && c != nil && strings.TrimSpace(c.Name) != "" {
		courseName = c.Name
	}
	if len(files) == 0 {
		if lang == "en" {
			return fmt.Sprintf("No files for %s.", courseName)
		}
		return fmt.Sprintf("%s 강의에 파일이 없습니다.", courseName)
	}
	if len(files) > limit {
		files = files[:limit]
	}

	header := fmt.Sprintf("Recent files for %s:", courseName)
	if lang == "ko" {
		header = fmt.Sprintf("%s 최근 파일:", courseName)
	}
	lines := []string{header}
	for _, f := range files {
		updated := f.UpdatedAt.In(time.FixedZone("KST", 9*3600)).Format("2006-01-02")
		lines = append(lines, fmt.Sprintf("- %s | %s | %d bytes", updated, f.DisplayName, f.Size))
	}
	return trimForTelegram(strings.Join(lines, "\n"))
}

func openBindingStore(ctx context.Context, cfg config) (*binding.Store, error) {
	if strings.TrimSpace(cfg.Database.URL) == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	store, err := binding.New(cfg.Database.URL)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureSchema(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func addChatCourseSubscription(ctx context.Context, cfg config, chatID string, courseID int) error {
	store, err := openBindingStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.AddChatCourse(ctx, chatID, courseID)
}

func removeChatCourseSubscription(ctx context.Context, cfg config, chatID string, courseID int) error {
	store, err := openBindingStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.RemoveChatCourse(ctx, chatID, courseID)
}

func listChatCourseIDs(ctx context.Context, cfg config, chatID string) ([]int, error) {
	store, err := openBindingStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListChatCourses(ctx, chatID)
}

func filterCoursesByIDs(courses []canvas.Course, ids []int) []canvas.Course {
	if len(ids) == 0 {
		return courses
	}
	filter := make(map[int]bool, len(ids))
	for _, id := range ids {
		filter[id] = true
	}
	var out []canvas.Course
	for _, c := range courses {
		if filter[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

func currentSemesterKeyKST(now time.Time) string {
	kst := now.In(time.FixedZone("KST", 9*3600))
	semester := 1
	if int(kst.Month()) >= 8 {
		semester = 2
	}
	return fmt.Sprintf("%04d-%d", kst.Year(), semester)
}

func courseMatchesSemester(course canvas.Course, semesterKey string) bool {
	targets := []string{
		strings.ToLower(course.Name),
		strings.ToLower(course.CourseCode),
	}
	patterns := []string{
		strings.ToLower(semesterKey),
		strings.ToLower(strings.ReplaceAll(semesterKey, "-", " ")),
		strings.ToLower(strings.ReplaceAll(semesterKey, "-", "_")),
	}
	for _, t := range targets {
		for _, p := range patterns {
			if p != "" && strings.Contains(t, p) {
				return true
			}
		}
	}
	return false
}

func filterCoursesBySemester(courses []canvas.Course, semesterKey string) []canvas.Course {
	var out []canvas.Course
	for _, c := range courses {
		if courseMatchesSemester(c, semesterKey) {
			out = append(out, c)
		}
	}
	return out
}

func semesterBoundsKST(now time.Time) (time.Time, time.Time) {
	kst := time.FixedZone("KST", 9*3600)
	base := now.In(kst)
	year, month := base.Year(), base.Month()
	if int(month) >= 8 {
		start := time.Date(year, time.August, 1, 0, 0, 0, 0, kst)
		end := time.Date(year+1, time.January, 1, 0, 0, 0, 0, kst)
		return start, end
	}
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, kst)
	end := time.Date(year, time.August, 1, 0, 0, 0, 0, kst)
	return start, end
}

func filterCoursesBySemesterWindow(courses []canvas.Course, now time.Time) []canvas.Course {
	start, end := semesterBoundsKST(now)
	var out []canvas.Course
	for _, c := range courses {
		if c.StartAt != nil {
			if c.StartAt.Before(end) && (c.EndAt == nil || !c.EndAt.Before(start)) {
				out = append(out, c)
			}
			continue
		}
		if c.EndAt != nil && !c.EndAt.Before(start) && c.EndAt.Before(end) {
			out = append(out, c)
		}
	}
	return out
}

func enrollmentTermIDs(courses []canvas.Course) map[int]bool {
	ids := make(map[int]bool)
	for _, c := range courses {
		if c.EnrollmentTermID > 0 {
			ids[c.EnrollmentTermID] = true
		}
	}
	return ids
}

func filterCoursesByEnrollmentTerms(courses []canvas.Course, termIDs map[int]bool) []canvas.Course {
	if len(termIDs) == 0 {
		return nil
	}
	var out []canvas.Course
	for _, c := range courses {
		if termIDs[c.EnrollmentTermID] {
			out = append(out, c)
		}
	}
	return out
}

func defaultSemesterCoursesForKey(courses []canvas.Course, semesterKey string) []canvas.Course {
	// Prefer date overlap for the current semester window (language-agnostic).
	windowMatches := filterCoursesBySemesterWindow(courses, time.Now())
	if len(windowMatches) > 0 {
		return windowMatches
	}

	filtered := filterCoursesBySemester(courses, semesterKey)
	if len(filtered) == 0 {
		return courses
	}

	// Expand to all matching enrollment terms, not just one dominant term.
	termIDs := enrollmentTermIDs(filtered)
	if len(termIDs) == 0 {
		return filtered
	}
	termCourses := filterCoursesByEnrollmentTerms(courses, termIDs)
	if len(termCourses) == 0 {
		return filtered
	}
	return termCourses
}

func defaultSemesterCourses(courses []canvas.Course) ([]canvas.Course, string) {
	semesterKey := currentSemesterKeyKST(time.Now())
	return defaultSemesterCoursesForKey(courses, semesterKey), semesterKey
}

func availableCourses(ctx context.Context, cfg config, client *canvas.Client) ([]canvas.Course, error) {
	courses, err := client.GetCourses(ctx)
	if err != nil {
		return nil, err
	}
	return filterCoursesByIDs(courses, cfg.Monitor.Courses), nil
}

func monitoredCourses(ctx context.Context, cfg config, client *canvas.Client, chatID string) ([]canvas.Course, error) {
	courses, err := availableCourses(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Database.URL) == "" || chatID == "" {
		def, _ := defaultSemesterCourses(courses)
		return def, nil
	}

	subIDs, err := listChatCourseIDs(ctx, cfg, chatID)
	if err != nil {
		def, _ := defaultSemesterCourses(courses)
		return def, nil
	}
	if len(subIDs) == 0 {
		def, _ := defaultSemesterCourses(courses)
		return def, nil
	}
	return filterCoursesByIDs(courses, subIDs), nil
}

func cmdListening(ctx context.Context, cfg config, client *canvas.Client, chatID, lang string) string {
	if strings.TrimSpace(cfg.Database.URL) == "" {
		return msg(lang, "DATABASE_URL이 필요합니다.", "DATABASE_URL is required.")
	}

	ids, err := listChatCourseIDs(ctx, cfg, chatID)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}
	if len(ids) == 0 {
		if client == nil {
			return msg(lang, "기본 구독 학기를 확인할 수 없습니다. /listen 으로 추가하세요.", "Cannot resolve default semester courses. Add one with /listen.")
		}
		courses, err := availableCourses(ctx, cfg, client)
		if err != nil {
			return msg(lang, "오류: ", "Error: ") + err.Error()
		}
		defaultCourses, semesterKey := defaultSemesterCourses(courses)
		lines := []string{
			msg(
				lang,
				fmt.Sprintf("명시적 구독이 없어 %s 학기 강의를 기본 구독으로 사용합니다.", semesterKey),
				fmt.Sprintf("No explicit subscriptions; using %s semester courses by default.", semesterKey),
			),
		}
		for _, c := range defaultCourses {
			lines = append(lines, fmt.Sprintf("- %d | %s", c.ID, c.Name))
		}
		return trimForTelegram(strings.Join(lines, "\n"))
	}

	if !hasCanvasConfig(cfg) {
		lines := []string{msg(lang, "구독 강의 ID:", "Subscribed course IDs:")}
		for _, id := range ids {
			lines = append(lines, fmt.Sprintf("- %d", id))
		}
		return strings.Join(lines, "\n")
	}

	courses, err := availableCourses(ctx, cfg, client)
	if err != nil {
		return msg(lang, "오류: ", "Error: ") + err.Error()
	}
	nameByID := make(map[int]string, len(courses))
	for _, c := range courses {
		nameByID[c.ID] = c.Name
	}

	lines := []string{msg(lang, "구독 중인 강의:", "Subscribed courses:")}
	for _, id := range ids {
		name := nameByID[id]
		if name == "" {
			name = strconv.Itoa(id)
		}
		lines = append(lines, fmt.Sprintf("- %d | %s", id, name))
	}
	return trimForTelegram(strings.Join(lines, "\n"))
}

func cmdListenSelector(ctx context.Context, cfg config, client *canvas.Client, page int, lang string) (botResponse, error) {
	courses, err := availableCourses(ctx, cfg, client)
	if err != nil {
		return botResponse{}, err
	}
	if len(courses) == 0 {
		return botResponse{Text: msg(lang, "강의가 없습니다.", "No courses found.")}, nil
	}

	pageItems, page, totalPages := paginateCourses(courses, page, 8)
	kb := &telegramInlineMarkup{}
	for _, c := range pageItems {
		label := c.Name
		if len([]rune(label)) > 48 {
			label = string([]rune(label)[:48]) + "..."
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []telegramInlineButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("sub:%d", c.ID),
		}})
	}
	appendPaginationRow(kb, lang, page, totalPages, "subsel")
	appendMenuRow(kb, lang)
	return botResponse{Text: selectorPrompt(lang, msg(lang, "알림을 받을 강의를 선택하세요.", "Select a course to subscribe for alerts."), page, totalPages), Keyboard: kb}, nil
}

func cmdUnlistenSelector(ctx context.Context, cfg config, client *canvas.Client, chatID string, page int, lang string) (botResponse, error) {
	ids, err := listChatCourseIDs(ctx, cfg, chatID)
	if err != nil {
		return botResponse{}, err
	}
	if len(ids) == 0 {
		return botResponse{Text: msg(lang, "구독 중인 강의가 없습니다.", "No subscribed courses.")}, nil
	}

	courses, err := availableCourses(ctx, cfg, client)
	if err != nil {
		return botResponse{}, err
	}
	nameByID := make(map[int]string, len(courses))
	for _, c := range courses {
		nameByID[c.ID] = c.Name
	}

	pageIDs, page, totalPages := paginateCourseIDs(ids, page, 8)
	kb := &telegramInlineMarkup{}
	for _, id := range pageIDs {
		label := nameByID[id]
		if label == "" {
			label = fmt.Sprintf("Course %d", id)
		}
		if len([]rune(label)) > 48 {
			label = string([]rune(label)[:48]) + "..."
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []telegramInlineButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("unsub:%d", id),
		}})
	}
	appendPaginationRow(kb, lang, page, totalPages, "unsubsel")
	appendMenuRow(kb, lang)
	return botResponse{Text: selectorPrompt(lang, msg(lang, "구독 해제할 강의를 선택하세요.", "Select a course to unsubscribe."), page, totalPages), Keyboard: kb}, nil
}

func botHelpMessage(lang string) string {
	if lang == "en" {
		return strings.Join([]string{
			"Available commands:",
			"/menu",
			"/status",
			"/settings",
			"/listening",
			"/listen",
			"/unlisten",
			"/courses [keyword]",
			"/assignments (interactive course selector)",
			"/upcoming [days] [limit]",
			"/announcements [limit]",
			"/files (interactive course selector)",
			"/chat <message> (Codex)",
			"or just send plain text to chat with Codex",
			"/bind",
		}, "\n")
	}
	return strings.Join([]string{
		"사용 가능한 명령어:",
		"/menu",
		"/status",
		"/settings",
		"/listening",
		"/listen",
		"/unlisten",
		"/courses [키워드]",
		"/assignments (강의 선택기)",
		"/upcoming [days] [limit]",
		"/announcements [limit]",
		"/files (강의 선택기)",
		"/chat <메시지> (Codex)",
		"또는 일반 텍스트를 보내 Codex와 대화",
		"/bind",
	}, "\n")
}

func languageSettingsKeyboard(lang string) *telegramInlineMarkup {
	return &telegramInlineMarkup{InlineKeyboard: [][]telegramInlineButton{
		{
			{Text: langLabel(lang, "한국어", "Korean"), CallbackData: "lang:ko"},
			{Text: langLabel(lang, "English", "English"), CallbackData: "lang:en"},
		},
		{
			{Text: msg(lang, "메뉴", "Menu"), CallbackData: "menu:home"},
		},
	}}
}

func botMenuMessage(lang string) string {
	if lang == "en" {
		return "Quick actions:"
	}
	return "빠른 메뉴:"
}

func mainMenuKeyboard(lang string) *telegramInlineMarkup {
	return &telegramInlineMarkup{InlineKeyboard: [][]telegramInlineButton{
		{
			{Text: msg(lang, "과제", "Assignments"), CallbackData: "menu:asgsel:0"},
			{Text: msg(lang, "파일", "Files"), CallbackData: "menu:filsel:0"},
		},
		{
			{Text: msg(lang, "다가올 과제", "Upcoming"), CallbackData: "menu:upcoming"},
			{Text: msg(lang, "공지", "Announcements"), CallbackData: "menu:ann"},
		},
		{
			{Text: msg(lang, "구독 보기", "Subscriptions"), CallbackData: "menu:listening"},
			{Text: msg(lang, "강의 목록", "Courses"), CallbackData: "menu:courses"},
		},
		{
			{Text: msg(lang, "구독 추가", "Subscribe"), CallbackData: "menu:subsel:0"},
			{Text: msg(lang, "구독 해제", "Unsubscribe"), CallbackData: "menu:unsubsel:0"},
		},
		{
			{Text: msg(lang, "상태", "Status"), CallbackData: "menu:status"},
			{Text: msg(lang, "설정", "Settings"), CallbackData: "menu:settings"},
		},
		{
			{Text: msg(lang, "관리자 열기", "Open Admin"), URL: adminDashboardURL()},
		},
	}}
}

func appendMenuRow(kb *telegramInlineMarkup, lang string) {
	kb.InlineKeyboard = append(kb.InlineKeyboard, []telegramInlineButton{{
		Text:         msg(lang, "메뉴", "Menu"),
		CallbackData: "menu:home",
	}})
}

func appendPaginationRow(kb *telegramInlineMarkup, lang string, page, totalPages int, actionPrefix string) {
	if totalPages <= 1 {
		return
	}
	var row []telegramInlineButton
	if page > 0 {
		row = append(row, telegramInlineButton{
			Text:         msg(lang, "이전", "Prev"),
			CallbackData: fmt.Sprintf("menu:%s:%d", actionPrefix, page-1),
		})
	}
	row = append(row, telegramInlineButton{
		Text:         fmt.Sprintf("%d/%d", page+1, totalPages),
		CallbackData: "menu:noop",
	})
	if page < totalPages-1 {
		row = append(row, telegramInlineButton{
			Text:         msg(lang, "다음", "Next"),
			CallbackData: fmt.Sprintf("menu:%s:%d", actionPrefix, page+1),
		})
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, row)
}

func selectorPrompt(lang, title string, page, totalPages int) string {
	if totalPages <= 1 {
		return title
	}
	return fmt.Sprintf("%s (%d/%d)", title, page+1, totalPages)
}

func paginateCourses(courses []canvas.Course, page, pageSize int) ([]canvas.Course, int, int) {
	if pageSize <= 0 {
		pageSize = 8
	}
	totalPages := (len(courses) + pageSize - 1) / pageSize
	if totalPages <= 0 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * pageSize
	end := start + pageSize
	if end > len(courses) {
		end = len(courses)
	}
	return courses[start:end], page, totalPages
}

func paginateCourseIDs(ids []int, page, pageSize int) ([]int, int, int) {
	if pageSize <= 0 {
		pageSize = 8
	}
	totalPages := (len(ids) + pageSize - 1) / pageSize
	if totalPages <= 0 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * pageSize
	end := start + pageSize
	if end > len(ids) {
		end = len(ids)
	}
	return ids[start:end], page, totalPages
}

func parseSelectorPage(raw string) int {
	page, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || page < 0 {
		return 0
	}
	return page
}

func botStatusMessage(ctx context.Context, cfg config, chatID, lang string, logger *slog.Logger) string {
	filter := "all"
	if len(cfg.Monitor.Courses) > 0 {
		filter = strings.Trim(strings.Replace(fmt.Sprint(cfg.Monitor.Courses), " ", ",", -1), "[]")
	}
	subscriptions := "all"
	if strings.TrimSpace(cfg.Database.URL) != "" {
		if ids, err := listChatCourseIDs(ctx, cfg, chatID); err == nil && len(ids) > 0 {
			subscriptions = strings.Trim(strings.Replace(fmt.Sprint(ids), " ", ",", -1), "[]")
		}
	}
	canvasReady := "false"
	if c, err := resolveCanvasClientForChat(ctx, cfg, chatID, logger); err == nil && c != nil {
		canvasReady = "true"
	}
	if lang == "en" {
		return fmt.Sprintf("chat_id=%s\ncanvas_ready=%s\nmonitor_courses=%s\nsubscribed_courses=%s\npoll_interval=%s\nlang=%s", chatID, canvasReady, filter, subscriptions, cfg.Monitor.PollInterval, lang)
	}
	return fmt.Sprintf("chat_id=%s\ncanvas_ready=%s\nmonitor_courses=%s\nsubscribed_courses=%s\npoll_interval=%s\n언어=%s", chatID, canvasReady, filter, subscriptions, cfg.Monitor.PollInterval, lang)
}

func langLabel(lang, ko, en string) string {
	if lang == "en" {
		return en
	}
	return ko
}

func msg(lang, ko, en string) string {
	if lang == "en" {
		return en
	}
	return ko
}

func adminDashboardURL() string {
	if v := strings.TrimSpace(os.Getenv("ADMIN_DASHBOARD_URL")); v != "" {
		return v
	}
	return "https://admin-dashboard-production-da11.up.railway.app"
}

func adminBackendURL() string {
	if v := strings.TrimSpace(os.Getenv("ADMIN_BACKEND_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(adminDashboardURL(), "/")
}

func adminBackendBotToken() string {
	return strings.TrimSpace(os.Getenv("ADMIN_BACKEND_BOT_TOKEN"))
}

func requestCodexReply(ctx context.Context, chatID, message, lang string) (string, error) {
	payload := map[string]string{
		"chatId":  chatID,
		"message": message,
		"lang":    lang,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adminBackendURL()+"/api/codex/chat", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := adminBackendBotToken(); token != "" {
		req.Header.Set("X-Admin-Bot-Token", token)
	}

	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed struct {
		OK    bool   `json:"ok"`
		Reply string `json:"reply"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode >= 400 || !parsed.OK {
		if parsed.Error != "" {
			return "", errors.New(parsed.Error)
		}
		return "", fmt.Errorf("admin backend error (%d)", resp.StatusCode)
	}
	if strings.TrimSpace(parsed.Reply) == "" {
		return "", errors.New("empty reply")
	}
	return trimForTelegram(parsed.Reply), nil
}

func canvasNotConfiguredResponse(lang string) botResponse {
	text := msg(
		lang,
		"Canvas가 아직 연결되지 않았습니다.\n- 원인: 이 chat_id에 바인딩된 Canvas 토큰이 없습니다.\n- 해결: 관리자 대시보드에서 토큰을 연결하거나 /bind 를 실행하세요.",
		"Canvas is not connected yet.\n- Cause: no Canvas token is bound for this chat_id.\n- Fix: connect token in Admin Dashboard or run /bind.",
	)
	return botResponse{
		Text: text,
		Keyboard: &telegramInlineMarkup{
			InlineKeyboard: [][]telegramInlineButton{{
				{Text: msg(lang, "관리자 열기", "Open Admin"), URL: adminDashboardURL()},
			}},
		},
	}
}

func resolveCanvasTokenForChat(ctx context.Context, cfg config, chatID string) (string, error) {
	if strings.TrimSpace(cfg.Canvas.Token) != "" {
		return strings.TrimSpace(cfg.Canvas.Token), nil
	}
	if strings.TrimSpace(cfg.Database.URL) == "" || strings.TrimSpace(chatID) == "" {
		return "", nil
	}

	store, err := binding.New(cfg.Database.URL)
	if err != nil {
		return "", err
	}
	defer store.Close()

	if err := store.EnsureSchema(ctx); err != nil {
		return "", err
	}
	return store.LookupCanvasAPIKeyByChatID(ctx, chatID)
}

func resolveCanvasClientForChat(ctx context.Context, cfg config, chatID string, logger *slog.Logger) (*canvas.Client, error) {
	if strings.TrimSpace(cfg.Canvas.URL) == "" {
		return nil, nil
	}
	token, err := resolveCanvasTokenForChat(ctx, cfg, chatID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	return canvas.NewClient(cfg.Canvas.URL, token, logger), nil
}

func languageForChat(ctx context.Context, cfg config, chatID string) string {
	if strings.TrimSpace(cfg.Database.URL) == "" {
		return "ko"
	}

	store, err := binding.New(cfg.Database.URL)
	if err != nil {
		return "ko"
	}
	defer store.Close()

	if err := store.EnsureSchema(ctx); err != nil {
		return "ko"
	}

	lang, err := store.GetChatLanguage(ctx, chatID)
	if err != nil || lang == "" {
		return "ko"
	}
	return lang
}

func setChatLanguage(ctx context.Context, cfg config, chatID, lang string) error {
	if strings.TrimSpace(cfg.Database.URL) == "" {
		return errors.New("DATABASE_URL is required")
	}

	store, err := binding.New(cfg.Database.URL)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.EnsureSchema(ctx); err != nil {
		return err
	}
	return store.SetChatLanguage(ctx, chatID, lang)
}

func sendTelegramBotMessage(ctx context.Context, botToken, chatID, message string, keyboard *telegramInlineMarkup) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	params := url.Values{
		"chat_id": {chatID},
		"text":    {trimForTelegram(message)},
	}
	if keyboard != nil {
		b, _ := json.Marshal(keyboard)
		params.Set("reply_markup", string(b))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram error: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func editTelegramBotMessage(ctx context.Context, botToken, chatID string, messageID int, message string, keyboard *telegramInlineMarkup) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	vals := url.Values{}
	vals.Set("chat_id", chatID)
	vals.Set("message_id", strconv.Itoa(messageID))
	vals.Set("text", trimForTelegram(message))
	vals.Set("disable_web_page_preview", "true")
	if keyboard != nil {
		b, _ := json.Marshal(keyboard)
		vals.Set("reply_markup", string(b))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(vals.Encode()))
	if err != nil {
		return fmt.Errorf("telegram edit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("telegram edit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram edit error: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func syncTelegramCommands(ctx context.Context, botToken string) error {
	commandsEn := []telegramBotCommand{
		{Command: "menu", Description: "Open quick actions"},
		{Command: "status", Description: "Show bot status"},
		{Command: "settings", Description: "Language settings"},
		{Command: "listening", Description: "Show subscriptions"},
		{Command: "listen", Description: "Subscribe to a course"},
		{Command: "unlisten", Description: "Unsubscribe from a course"},
		{Command: "courses", Description: "Show course list"},
		{Command: "assignments", Description: "Pick course assignments"},
		{Command: "upcoming", Description: "Upcoming due assignments"},
		{Command: "announcements", Description: "Recent announcements"},
		{Command: "files", Description: "Pick course files"},
		{Command: "chat", Description: "Ask Codex"},
		{Command: "bind", Description: "Bind Canvas token"},
		{Command: "help", Description: "Show help"},
	}
	commandsKo := []telegramBotCommand{
		{Command: "menu", Description: "빠른 메뉴 열기"},
		{Command: "status", Description: "상태 보기"},
		{Command: "settings", Description: "언어 설정"},
		{Command: "listening", Description: "구독 목록"},
		{Command: "listen", Description: "강의 구독"},
		{Command: "unlisten", Description: "강의 구독 해제"},
		{Command: "courses", Description: "강의 목록"},
		{Command: "assignments", Description: "강의 과제 선택"},
		{Command: "upcoming", Description: "다가올 과제"},
		{Command: "announcements", Description: "최근 공지"},
		{Command: "files", Description: "강의 파일 선택"},
		{Command: "chat", Description: "Codex에게 질문"},
		{Command: "bind", Description: "Canvas 토큰 바인딩"},
		{Command: "help", Description: "도움말"},
	}
	if err := setTelegramCommands(ctx, botToken, "", commandsEn); err != nil {
		return err
	}
	if err := setTelegramCommands(ctx, botToken, "ko", commandsKo); err != nil {
		return err
	}
	return nil
}

func setTelegramCommands(ctx context.Context, botToken, languageCode string, commands []telegramBotCommand) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", botToken)
	payload := map[string]any{
		"commands": commands,
	}
	if languageCode != "" {
		payload["language_code"] = languageCode
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("setMyCommands request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("setMyCommands send: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("setMyCommands error %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("setMyCommands decode: %w", err)
	}
	if !parsed.OK {
		if strings.TrimSpace(parsed.Description) == "" {
			return errors.New("setMyCommands returned ok=false")
		}
		return errors.New(parsed.Description)
	}
	return nil
}

func answerTelegramCallbackQuery(ctx context.Context, botToken, callbackQueryID, text string) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", botToken)
	params := url.Values{"callback_query_id": {callbackQueryID}}
	if strings.TrimSpace(text) != "" {
		params.Set("text", text)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("answerCallbackQuery error: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func trimCallbackNotice(message string) string {
	const maxChars = 180
	r := []rune(strings.TrimSpace(message))
	if len(r) <= maxChars {
		return string(r)
	}
	return string(r[:maxChars])
}

func trimForTelegram(message string) string {
	const maxChars = 3800
	r := []rune(message)
	if len(r) <= maxChars {
		return message
	}
	return string(r[:maxChars]) + "\n...[truncated]"
}

func getTelegramUpdates(ctx context.Context, botToken string, offset int64, timeoutSec int) ([]telegramBotUpdate, error) {
	params := url.Values{
		"offset":  {strconv.FormatInt(offset, 10)},
		"timeout": {strconv.Itoa(timeoutSec)},
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?%s", botToken, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build getUpdates request: %w", err)
	}

	httpClient := &http.Client{Timeout: time.Duration(timeoutSec+10) * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getUpdates request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("getUpdates error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed telegramGetUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode getUpdates response: %w", err)
	}
	if !parsed.OK {
		return nil, errors.New("getUpdates returned ok=false")
	}
	return parsed.Result, nil
}
