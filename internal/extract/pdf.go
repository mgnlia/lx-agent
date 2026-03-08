// Package extract provides PDF text extraction with OCR fallback.
//
// Strategy:
//  1. Try fast text extraction via pdfium (text-based PDFs)
//  2. If result is empty/garbled, fall back to kreuzberg CLI (OCR)
package extract

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"
)

// PDFToText extracts text from a PDF file. It first tries kreuzberg
// (handles both image-based and text-based PDFs). Falls back to
// basic text extraction if kreuzberg is not installed.
func PDFToText(ctx context.Context, pdfPath string) (string, error) {
	// Try kreuzberg first (best quality, handles image PDFs)
	text, err := kreuzbergExtract(ctx, pdfPath)
	if err == nil && isUsableText(text) {
		return text, nil
	}

	// Fallback: try python pypdfium2
	text, err = pypdfiumExtract(ctx, pdfPath)
	if err == nil && isUsableText(text) {
		return text, nil
	}

	return "", fmt.Errorf("could not extract text from %s (tried kreuzberg, pypdfium2)", pdfPath)
}

func kreuzbergExtract(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "kreuzberg", "extract", path, "--format", "text")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func pypdfiumExtract(ctx context.Context, path string) (string, error) {
	script := fmt.Sprintf(`
import pypdfium2 as pdfium
doc = pdfium.PdfDocument("%s")
for p in doc:
    print(p.get_textpage().get_text_range())
`, path)
	cmd := exec.CommandContext(ctx, "python3", "-c", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// isUsableText checks if extracted text is meaningful (not empty or garbled).
func isUsableText(text string) bool {
	if len(strings.TrimSpace(text)) < 50 {
		return false
	}
	// Count printable vs non-printable characters
	printable := 0
	total := 0
	for _, r := range text {
		total++
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return float64(printable)/float64(total) > 0.8
}
