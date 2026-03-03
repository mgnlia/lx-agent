package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/mgnlia/lx-agent/internal/canvas"
	"github.com/mgnlia/lx-agent/internal/notifier"
	"github.com/mgnlia/lx-agent/internal/summarizer"
)

type Config struct {
	PollInterval  time.Duration
	CourseFilter  []int // empty = all courses
	SummarizeNew  bool
	DeadlineAlerts []int // days before due (e.g., [3, 1, 0])
	StatePath     string
}

type Monitor struct {
	client     *canvas.Client
	notifier   notifier.Notifier
	summarizer summarizer.Summarizer
	config     Config
	state      *State
	logger     *slog.Logger
}

func New(
	client *canvas.Client,
	n notifier.Notifier,
	s summarizer.Summarizer,
	cfg Config,
	logger *slog.Logger,
) *Monitor {
	if cfg.StatePath == "" {
		cfg.StatePath = "lx-state.json"
	}
	if len(cfg.DeadlineAlerts) == 0 {
		cfg.DeadlineAlerts = []int{3, 1, 0}
	}

	state := NewState(cfg.StatePath)
	state.Load()

	return &Monitor{
		client:     client,
		notifier:   n,
		summarizer: s,
		config:     cfg,
		state:      state,
		logger:     logger,
	}
}

func (m *Monitor) Run(ctx context.Context) error {
	m.logger.Info("starting monitor", "interval", m.config.PollInterval)

	// Initial check
	if err := m.check(ctx); err != nil {
		m.logger.Error("check failed", "err", err)
	}

	ticker := time.NewTicker(m.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("monitor stopped")
			return ctx.Err()
		case <-ticker.C:
			if err := m.check(ctx); err != nil {
				m.logger.Error("check failed", "err", err)
			}
		}
	}
}

func (m *Monitor) RunOnce(ctx context.Context) error {
	return m.check(ctx)
}

func (m *Monitor) check(ctx context.Context) error {
	m.logger.Info("running check")

	courses, err := m.client.GetCourses(ctx)
	if err != nil {
		return fmt.Errorf("get courses: %w", err)
	}

	// Filter courses if configured
	if len(m.config.CourseFilter) > 0 {
		filterSet := make(map[int]bool)
		for _, id := range m.config.CourseFilter {
			filterSet[id] = true
		}
		var filtered []canvas.Course
		for _, c := range courses {
			if filterSet[c.ID] {
				filtered = append(filtered, c)
			}
		}
		courses = filtered
	}

	m.logger.Info("checking courses", "count", len(courses))

	var messages []string

	for _, course := range courses {
		// Check new files
		newFiles, err := m.checkFiles(ctx, course)
		if err != nil {
			m.logger.Error("check files failed", "course", course.Name, "err", err)
		}
		messages = append(messages, newFiles...)

		// Check new assignments
		newAssignments, err := m.checkAssignments(ctx, course)
		if err != nil {
			m.logger.Error("check assignments failed", "course", course.Name, "err", err)
		}
		messages = append(messages, newAssignments...)

		// Check deadlines
		deadlines, err := m.checkDeadlines(ctx, course)
		if err != nil {
			m.logger.Error("check deadlines failed", "course", course.Name, "err", err)
		}
		messages = append(messages, deadlines...)
	}

	// Check announcements (all courses at once)
	courseIDs := make([]int, len(courses))
	for i, c := range courses {
		courseIDs[i] = c.ID
	}
	if len(courseIDs) > 0 {
		newAnnouncements, err := m.checkAnnouncements(ctx, courseIDs, courses)
		if err != nil {
			m.logger.Error("check announcements failed", "err", err)
		}
		messages = append(messages, newAnnouncements...)
	}

	// Send notifications
	for _, msg := range messages {
		if err := m.notifier.Send(ctx, msg); err != nil {
			m.logger.Error("notify failed", "err", err)
		}
	}

	m.state.Data.LastCheck = time.Now()
	m.state.Save()

	if len(messages) > 0 {
		m.logger.Info("sent notifications", "count", len(messages))
	} else {
		m.logger.Info("no updates")
	}

	return nil
}

func (m *Monitor) checkFiles(ctx context.Context, course canvas.Course) ([]string, error) {
	files, err := m.client.GetFiles(ctx, course.ID)
	if err != nil {
		return nil, err
	}

	var messages []string
	for _, f := range files {
		if !m.state.IsFileNew(f.ID, f.Size) {
			continue
		}
		m.state.MarkFile(f.ID, f.Size)

		msg := fmt.Sprintf("📄 *새 강의자료*\n📚 %s\n📎 %s (%s)",
			course.Name, f.DisplayName, humanSize(f.Size))

		if m.summarizer != nil && m.config.SummarizeNew {
			summary, err := m.summarizeFile(ctx, f)
			if err != nil {
				m.logger.Warn("summarize failed", "file", f.DisplayName, "err", err)
			} else if summary != "" {
				msg += "\n\n📝 *요약:*\n" + summary
			}
		}

		messages = append(messages, msg)
	}

	return messages, nil
}

func (m *Monitor) checkAssignments(ctx context.Context, course canvas.Course) ([]string, error) {
	assignments, err := m.client.GetAssignments(ctx, course.ID)
	if err != nil {
		return nil, err
	}

	var messages []string
	for _, a := range assignments {
		if !m.state.IsAssignmentNew(a.ID) {
			continue
		}
		m.state.MarkAssignment(a.ID)

		due := "마감일 없음"
		if a.DueAt != nil {
			due = a.DueAt.In(time.FixedZone("KST", 9*3600)).Format("2006-01-02 15:04 KST")
		}

		msg := fmt.Sprintf("📝 *새 과제*\n📚 %s\n📌 %s\n⏰ %s\n💯 %.0f점",
			course.Name, a.Name, due, a.PointsPossible)

		messages = append(messages, msg)
	}

	return messages, nil
}

func (m *Monitor) checkDeadlines(ctx context.Context, course canvas.Course) ([]string, error) {
	assignments, err := m.client.GetAssignments(ctx, course.ID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var messages []string

	for _, a := range assignments {
		if a.DueAt == nil || a.Submitted {
			continue
		}

		until := time.Until(*a.DueAt)
		if until < 0 {
			continue
		}

		daysLeft := int(until.Hours() / 24)

		for _, alertDay := range m.config.DeadlineAlerts {
			if daysLeft <= alertDay {
				level := fmt.Sprintf("D-%d", alertDay)
				if alertDay == 0 {
					level = "D-DAY"
				}

				if !m.state.ShouldAlertDeadline(a.ID, level) {
					continue
				}
				m.state.MarkDeadlineAlert(a.ID, level)

				kst := a.DueAt.In(time.FixedZone("KST", 9*3600))
				hoursLeft := int(until.Hours())

				var urgency string
				if alertDay == 0 {
					urgency = fmt.Sprintf("🔴 *%s* — %d시간 남음!", level, hoursLeft)
				} else if alertDay == 1 {
					urgency = fmt.Sprintf("🟡 *%s* — 내일 마감!", level)
				} else {
					urgency = fmt.Sprintf("🟢 *%s* — %d일 남음", level, daysLeft)
				}

				msg := fmt.Sprintf("⏰ *과제 마감 알림*\n📚 %s\n📌 %s\n%s\n📅 %s",
					course.Name, a.Name, urgency,
					kst.Format("2006-01-02 15:04 KST"))

				_ = now
				messages = append(messages, msg)
				break
			}
		}
	}

	return messages, nil
}

func (m *Monitor) checkAnnouncements(ctx context.Context, courseIDs []int, courses []canvas.Course) ([]string, error) {
	announcements, err := m.client.GetAnnouncements(ctx, courseIDs)
	if err != nil {
		return nil, err
	}

	courseMap := make(map[string]string)
	for _, c := range courses {
		courseMap[fmt.Sprintf("course_%d", c.ID)] = c.Name
	}

	var messages []string
	for _, a := range announcements {
		if !m.state.IsAnnouncementNew(a.ID) {
			continue
		}
		m.state.MarkAnnouncement(a.ID)

		courseName := courseMap[a.ContextCode]
		if courseName == "" {
			courseName = a.ContextCode
		}

		// Strip HTML tags from message
		plainMsg := stripHTML(a.Message)
		if len(plainMsg) > 500 {
			plainMsg = plainMsg[:500] + "..."
		}

		msg := fmt.Sprintf("📢 *새 공지*\n📚 %s\n📌 %s\n\n%s",
			courseName, a.Title, plainMsg)

		if m.summarizer != nil && len(a.Message) > 200 {
			summary, err := m.summarizer.SummarizeText(ctx, a.Title, plainMsg)
			if err == nil && summary != "" {
				msg += "\n\n📝 *요약:*\n" + summary
			}
		}

		messages = append(messages, msg)
	}

	return messages, nil
}

func (m *Monitor) summarizeFile(ctx context.Context, f canvas.File) (string, error) {
	// Only summarize reasonable file types and sizes
	lower := strings.ToLower(f.DisplayName)
	if !strings.HasSuffix(lower, ".pdf") && !strings.HasSuffix(lower, ".pptx") &&
		!strings.HasSuffix(lower, ".txt") && !strings.HasSuffix(lower, ".md") &&
		!strings.HasSuffix(lower, ".docx") {
		return "", nil
	}

	if f.Size > 50*1024*1024 { // 50MB limit
		return "", nil
	}

	data, err := m.client.DownloadFile(ctx, f.URL)
	if err != nil {
		return "", err
	}

	return m.summarizer.SummarizeFile(ctx, f.DisplayName, data)
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "<p>", "\n")
	s = htmlTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func humanSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}
