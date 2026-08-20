package picker

import (
	"strings"
	"testing"
	"unicode"
)

func TestSanitizeTextRemovesTerminalControls(t *testing.T) {
	input := "name\x1b[31m\r\n\x00\x7f\u0085end"
	got := sanitizeText(input)
	for _, r := range got {
		if unicode.IsControl(r) {
			t.Fatalf("sanitized text contains control rune U+%04X: %q", r, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') || strings.ContainsAny(got, "\r\n") {
		t.Fatalf("sanitized text contains terminal control: %q", got)
	}
	if !strings.Contains(got, "[31m") {
		t.Fatalf("expected harmless remainder of ANSI sequence to remain visible: %q", got)
	}
}

func TestRenderSanitizesSourceAndTitleText(t *testing.T) {
	model := newModel([]Entry{{
		ID:   "id",
		Path: "path",
		Name: "unsafe\x1b[2J\nname",
		Type: EntryFile,
	}})
	model.dir = "dir\r\n\x1b[31m"
	screen := render(model, "title\x1b]0;owned\a", 80, 12)

	if strings.Contains(screen, "unsafe\x1b[2J") || strings.Contains(screen, "dir\r\n") || strings.Contains(screen, "title\x1b]0") {
		t.Fatalf("rendered screen contains unsanitized source text: %q", screen)
	}
	if !strings.HasPrefix(screen, "\x1b[H\x1b[2J") {
		t.Fatalf("rendered screen lacks picker clear sequence: %q", screen)
	}
}

func TestFitLineAccountsForWideRunes(t *testing.T) {
	got := fitLine("ab北大cd", 6)
	if got != "ab北…" {
		t.Fatalf("fitLine = %q, want %q", got, "ab北…")
	}
	if width := displayWidth(got); width != 5 {
		t.Fatalf("display width = %d, want 5", width)
	}
}

func TestFormatSize(t *testing.T) {
	tests := map[int64]string{
		0:           "0 B",
		1023:        "1023 B",
		1024:        "1.0 KiB",
		1024 * 1024: "1.0 MiB",
	}
	for size, want := range tests {
		if got := formatSize(size); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", size, got, want)
		}
	}
}
