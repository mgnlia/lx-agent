# PDF Text Extraction

## Problem

Many SNU course PDFs (especially lecture slides) are **image-based** — text is embedded as images, not selectable text. Standard PDF libraries (`pypdf`, `pypdfium2`) return empty or garbled output for these files.

## Solution: Kreuzberg

[Kreuzberg](https://github.com/kreuzberg-dev/kreuzberg) (v4.4.4+) is a polyglot document intelligence framework with a Rust core. It handles both text-based and image-based PDFs with OCR, extracting math notation, tables, and structured text.

### Install

```bash
# Via pipx (recommended, isolated env)
pipx install kreuzberg

# Or via pip
pip install kreuzberg
```

### CLI Usage

```bash
# Extract text from a PDF
kreuzberg extract lecture.pdf --format text

# JSON output with metadata
kreuzberg extract lecture.pdf --format json
```

### Python Usage

```python
from kreuzberg import extract_file

async def extract_pdf(path: str) -> str:
    result = await extract_file(path)
    return result.content
```

## Benchmark (tested on Mac Mini M4)

| Library | Image PDF | Text PDF | Speed |
|---------|-----------|----------|-------|
| **kreuzberg** | ✅ Full OCR | ✅ | ~5s/page |
| pypdfium2 | ❌ Empty | ✅ | 0.003s |
| PyMuPDF | ❌ Garbled | ✅ | 0.01s |
| pypdf | ❌ Empty | ✅ | 0.024s |

## Alternatives Considered

| Tool | Pros | Cons |
|------|------|------|
| **EasyOCR** | Good accuracy, free | Slower, Python only |
| **PaddleOCR** | Fast, accurate | Heavy dependencies |
| **OCRmyPDF** | Adds OCR layer to PDF | Tesseract-based, lower accuracy |
| **Tesseract** | Widely available | Poor on complex layouts |

## Integration Notes

- Kreuzberg is recommended for the `summarize_new` feature when processing image-based lecture PDFs
- For text-based PDFs, `pypdfium2` is faster and sufficient
- A hybrid approach (try text extraction first, fall back to kreuzberg for OCR) is optimal
