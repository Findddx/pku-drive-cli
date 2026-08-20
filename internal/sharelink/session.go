package sharelink

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
)

// Entry is a shared item addressed by a slash-separated path relative to the
// shared folder. Item retains the exact AnyShare identity used for download.
type Entry struct {
	Path string
	Name string
	Type string
	Size int64
	Item anyshare.Item
}

// Session is one isolated share-link browsing session.
type Session struct {
	linkType LinkType
	api      *anyshare.Client
	root     Entry

	mu    sync.RWMutex
	cache map[string][]Entry
}

func newSession(linkType LinkType, api *anyshare.Client, root Entry) *Session {
	return &Session{linkType: linkType, api: api, root: root, cache: make(map[string][]Entry)}
}

// Type returns the share's authorization model.
func (s *Session) Type() LinkType {
	if s == nil {
		return ""
	}
	return s.linkType
}

// Root returns the exact shared file or directory root.
func (s *Session) Root() Entry {
	if s == nil {
		return Entry{}
	}
	return s.root
}

// List returns the immediate children of relativeDir. An empty path names the
// shared directory root; nested directories are loaded only when requested.
func (s *Session) List(ctx context.Context, relativeDir string) ([]Entry, error) {
	parts, err := splitRelative(relativeDir, true)
	if err != nil {
		return nil, err
	}
	if s == nil || s.api == nil {
		return nil, invalidRemoteError("shared session is unavailable")
	}
	key := strings.Join(parts, "/")
	if entries, ok := s.cached(key); ok {
		return entries, nil
	}
	directory := s.root
	if len(parts) > 0 {
		directory, err = s.resolveParts(ctx, parts)
		if err != nil {
			return nil, err
		}
	}
	if directory.Type != "directory" {
		return nil, notDirectoryError()
	}
	items, err := s.api.ListFolder(ctx, itemID(directory.Item))
	if err != nil {
		return nil, classifyAPIError(err)
	}
	entries := make([]Entry, 0, len(items))
	names := make(map[string]struct{}, len(items))
	for _, item := range items {
		entry, err := makeChildEntry(key, item)
		if err != nil {
			return nil, err
		}
		if _, exists := names[entry.Name]; exists {
			return nil, invalidRemoteError("ambiguous directory entry")
		}
		names[entry.Name] = struct{}{}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Type == "directory" && entries[j].Type != "directory" {
			return true
		}
		if entries[i].Type != "directory" && entries[j].Type == "directory" {
			return false
		}
		return entries[i].Name < entries[j].Name
	})
	s.store(key, entries)
	return cloneEntries(entries), nil
}

// Resolve resolves one strict relative file path without recursively expanding
// unrelated directories.
func (s *Session) Resolve(ctx context.Context, relativeFile string) (Entry, error) {
	parts, err := splitRelative(relativeFile, false)
	if err != nil {
		return Entry{}, err
	}
	if s == nil || s.api == nil {
		return Entry{}, invalidRemoteError("shared session is unavailable")
	}
	entry, err := s.resolveParts(ctx, parts)
	if err != nil {
		return Entry{}, err
	}
	if entry.Type != "file" {
		return Entry{}, notFileError()
	}
	return entry, nil
}

func (s *Session) resolveParts(ctx context.Context, parts []string) (Entry, error) {
	if s.root.Type == "file" {
		if len(parts) == 1 && parts[0] == s.root.Name {
			return s.root, nil
		}
		return Entry{}, notFoundError()
	}
	currentDir := ""
	var current Entry
	for index, name := range parts {
		children, err := s.List(ctx, currentDir)
		if err != nil {
			return Entry{}, err
		}
		matches := 0
		for _, child := range children {
			if child.Name == name {
				current = child
				matches++
			}
		}
		if matches == 0 {
			return Entry{}, notFoundError()
		}
		if matches != 1 {
			return Entry{}, invalidRemoteError("ambiguous directory entry")
		}
		if index+1 < len(parts) {
			if current.Type != "directory" {
				return Entry{}, notDirectoryError()
			}
			currentDir = current.Path
		}
	}
	return current, nil
}

func makeRootEntry(item anyshare.Item) (Entry, error) {
	typeName, ok := normalizeItemType(item.Type)
	if !ok || !validItemIdentity(itemID(item)) {
		return Entry{}, invalidRemoteError("invalid shared root")
	}
	item.Type = typeName
	entry := Entry{Name: item.Name, Type: typeName, Size: item.Size, Item: item}
	if typeName == "file" {
		if !validSegment(item.Name) {
			return Entry{}, invalidRemoteError("invalid shared file name")
		}
		entry.Path = item.Name
	}
	return entry, nil
}

func makeChildEntry(parent string, item anyshare.Item) (Entry, error) {
	typeName, ok := normalizeItemType(item.Type)
	if !ok || !validItemIdentity(itemID(item)) || !validSegment(item.Name) {
		return Entry{}, invalidRemoteError("invalid shared directory entry")
	}
	item.Type = typeName
	path := item.Name
	if parent != "" {
		path = parent + "/" + item.Name
	}
	return Entry{Path: path, Name: item.Name, Type: typeName, Size: item.Size, Item: item}, nil
}

func splitRelative(value string, allowRoot bool) ([]string, error) {
	if value == "" {
		if allowRoot {
			return nil, nil
		}
		return nil, invalidPathError()
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") || strings.Contains(value, `\`) || strings.IndexByte(value, 0) >= 0 {
		return nil, invalidPathError()
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if !validSegment(part) {
			return nil, invalidPathError()
		}
	}
	return parts, nil
}

func validSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

func validItemIdentity(value string) bool {
	return strings.HasPrefix(value, "gns://") && len(value) > len("gns://")
}

func (s *Session) cached(key string) ([]Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, ok := s.cache[key]
	return cloneEntries(entries), ok
}

func (s *Session) store(key string, entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = cloneEntries(entries)
}

func cloneEntries(entries []Entry) []Entry {
	if entries == nil {
		return nil
	}
	return append([]Entry(nil), entries...)
}
