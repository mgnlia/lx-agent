package summarizer

import "context"

// Summarizer generates text summaries of content.
type Summarizer interface {
	// SummarizeText summarizes plain text content.
	SummarizeText(ctx context.Context, title, text string) (string, error)

	// SummarizeFile summarizes a file given its content bytes and filename.
	SummarizeFile(ctx context.Context, filename string, data []byte) (string, error)
}
