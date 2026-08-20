package picker

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const defaultTitle = "Select files to download"

func visibleRows(height int) int {
	const nonListRows = 6
	rows := height - nonListRows
	if rows < 1 {
		return 1
	}
	return rows
}

func render(m *model, title string, width, height int) string {
	if width < 20 {
		width = 20
	}
	if height < 7 {
		height = 7
	}
	rows := visibleRows(height)
	m.ensureVisible(rows)

	if title == "" {
		title = defaultTitle
	}
	dir := m.dir
	if dir == "" {
		dir = "/"
	}

	var output strings.Builder
	output.WriteString("\x1b[H\x1b[2J")
	output.WriteString(fitLine(sanitizeText(title), width))
	output.WriteString("\r\n")
	output.WriteString(fitLine("Location: "+sanitizeText(dir), width))
	output.WriteString("\r\n")
	output.WriteString(fitLine(fmt.Sprintf("Selected: %d", len(m.selected)), width))
	output.WriteString("\r\n\r\n")

	end := m.offset + rows
	if end > len(m.entries) {
		end = len(m.entries)
	}
	for index := m.offset; index < end; index++ {
		entry := m.entries[index]
		cursor := "  "
		if index == m.cursor {
			cursor = "> "
		}
		var marker string
		var suffix string
		if entry.Type == EntryDirectory {
			marker = "[D] "
			suffix = "/"
		} else if m.isSelected(entry) {
			marker = "[x] "
		} else {
			marker = "[ ] "
		}

		name := sanitizeText(entry.Name) + suffix
		line := cursor + marker + name
		if entry.Type == EntryFile && entry.Size >= 0 {
			size := formatSize(entry.Size)
			available := width - displayWidth(line) - displayWidth(size) - 2
			if available >= 1 {
				line += strings.Repeat(" ", available) + "  " + size
			}
		}
		output.WriteString(fitLine(line, width))
		output.WriteString("\r\n")
	}
	for index := end - m.offset; index < rows; index++ {
		output.WriteString("\r\n")
	}

	output.WriteString(fitLine("Arrows/j/k: move  l/right: open  h/left/backspace: back", width))
	output.WriteString("\r\n")
	output.WriteString(fitLine("Space: select file  Enter: confirm  q/Esc: cancel", width))
	return output.String()
}

func sanitizeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return '\uFFFD'
		}
		return r
	}, value)
}

func fitLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return truncateWidth(value, width-1) + "…"
}

func truncateWidth(value string, width int) string {
	if width <= 0 {
		return ""
	}
	var result strings.Builder
	used := 0
	for _, r := range value {
		runeWidth := displayRuneWidth(r)
		if used+runeWidth > width {
			break
		}
		result.WriteRune(r)
		used += runeWidth
	}
	return result.String()
}

func displayWidth(value string) int {
	width := 0
	for _, r := range value {
		width += displayRuneWidth(r)
	}
	return width
}

func displayRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f ||
		r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1faff) ||
		(r >= 0x20000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

func formatSize(size int64) string {
	const unit = int64(1024)
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	divisor := unit
	exponent := 0
	for quotient := size / unit; quotient >= unit && exponent < 5; quotient /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(divisor), "KMGTPE"[exponent])
}
