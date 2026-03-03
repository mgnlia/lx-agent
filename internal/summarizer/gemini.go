package summarizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type GeminiSummarizer struct {
	apiKey string
	model  string
}

func NewGemini(apiKey string) *GeminiSummarizer {
	return &GeminiSummarizer{
		apiKey: apiKey,
		model:  "gemini-2.5-flash",
	}
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string          `json:"text,omitempty"`
	InlineData *geminiInline   `json:"inline_data,omitempty"`
}

type geminiInline struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (g *GeminiSummarizer) SummarizeText(ctx context.Context, title, text string) (string, error) {
	prompt := fmt.Sprintf(`Summarize the following content concisely in Korean (한국어). Use bullet points for key topics.

Title: %s

Content:
%s`, title, truncate(text, 30000))

	return g.generate(ctx, prompt)
}

func (g *GeminiSummarizer) SummarizeFile(ctx context.Context, filename string, data []byte) (string, error) {
	// For text-based files, extract and summarize as text
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".md") ||
		strings.HasSuffix(lower, ".csv") || strings.HasSuffix(lower, ".json") {
		return g.SummarizeText(ctx, filename, string(data))
	}

	// For binary files, fall back to text extraction hint
	return g.SummarizeText(ctx, filename, fmt.Sprintf("[Binary file: %s, %d bytes — upload to Gemini for full analysis]", filename, len(data)))
}

func (g *GeminiSummarizer) generate(ctx context.Context, prompt string) (string, error) {
	reqBody := geminiRequest{
		Contents: []geminiContent{{
			Parts: []geminiPart{{Text: prompt}},
		}},
	}

	body, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", g.model, g.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini error %d: %s", resp.StatusCode, string(respBody))
	}

	var gr geminiResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	if gr.Error != nil {
		return "", fmt.Errorf("gemini: %s", gr.Error.Message)
	}

	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini: empty response")
	}

	return gr.Candidates[0].Content.Parts[0].Text, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...[truncated]"
}
