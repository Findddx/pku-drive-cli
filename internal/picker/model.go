package picker

import (
	"sort"
	"strings"
)

type key uint8

const (
	keyUnknown key = iota
	keyUp
	keyDown
	keyRight
	keyLeft
	keySpace
	keyEnter
	keyEscape
	keyQuit
	keyInterrupt
)

type effectKind uint8

const (
	effectNone effectKind = iota
	effectEnterDirectory
	effectLeaveDirectory
	effectConfirm
	effectCancel
	effectInterrupt
)

type effect struct {
	kind  effectKind
	entry Entry
}

type entryKey struct {
	path string
	id   string
}

type location struct {
	dir     string
	entries []Entry
	cursor  int
	offset  int
}

type model struct {
	location
	parents  []location
	selected map[entryKey]Entry
}

func newModel(entries []Entry) *model {
	return &model{
		location: location{entries: sortedEntries(entries)},
		selected: make(map[entryKey]Entry),
	}
}

func sortedEntries(entries []Entry) []Entry {
	result := append([]Entry(nil), entries...)
	sort.SliceStable(result, func(i, j int) bool {
		iDir := result[i].Type == EntryDirectory
		jDir := result[j].Type == EntryDirectory
		if iDir != jDir {
			return iDir
		}
		iName := strings.ToLower(sanitizeText(result[i].Name))
		jName := strings.ToLower(sanitizeText(result[j].Name))
		if iName != jName {
			return iName < jName
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func (m *model) handle(pressed key, rows int) effect {
	switch pressed {
	case keyUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case keyDown:
		if m.cursor+1 < len(m.entries) {
			m.cursor++
		}
	case keyRight:
		if entry, ok := m.current(); ok && entry.Type == EntryDirectory {
			return effect{kind: effectEnterDirectory, entry: entry}
		}
	case keyLeft:
		if len(m.parents) > 0 {
			return effect{kind: effectLeaveDirectory}
		}
	case keySpace:
		if entry, ok := m.current(); ok && entry.Type == EntryFile {
			entryKey := keyFor(entry)
			if _, exists := m.selected[entryKey]; exists {
				delete(m.selected, entryKey)
			} else {
				m.selected[entryKey] = entry
			}
		}
	case keyEnter:
		return effect{kind: effectConfirm}
	case keyEscape, keyQuit:
		return effect{kind: effectCancel}
	case keyInterrupt:
		return effect{kind: effectInterrupt}
	}
	m.ensureVisible(rows)
	return effect{kind: effectNone}
}

func (m *model) current() (Entry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return Entry{}, false
	}
	return m.entries[m.cursor], true
}

func (m *model) enter(dir string, entries []Entry) {
	m.parents = append(m.parents, m.location)
	m.location = location{dir: dir, entries: sortedEntries(entries)}
}

func (m *model) leave() {
	if len(m.parents) == 0 {
		return
	}
	last := len(m.parents) - 1
	m.location = m.parents[last]
	m.parents = m.parents[:last]
}

func (m *model) ensureVisible(rows int) {
	if rows < 1 {
		rows = 1
	}
	if len(m.entries) == 0 {
		m.cursor = 0
		m.offset = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.entries) {
		m.cursor = len(m.entries) - 1
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	maxOffset := len(m.entries) - rows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *model) selection() []Entry {
	result := make([]Entry, 0, len(m.selected))
	for _, entry := range m.selected {
		result = append(result, entry)
	}
	return result
}

func (m *model) isSelected(entry Entry) bool {
	_, ok := m.selected[keyFor(entry)]
	return ok
}

func keyFor(entry Entry) entryKey {
	return entryKey{path: entry.Path, id: entry.ID}
}
