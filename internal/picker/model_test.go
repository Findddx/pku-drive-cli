package picker

import (
	"reflect"
	"testing"
)

func TestModelSortsDirectoriesBeforeFilesAndScrolls(t *testing.T) {
	entries := []Entry{
		{ID: "f-z", Path: "z.txt", Name: "z.txt", Type: EntryFile},
		{ID: "d-b", Path: "Beta", Name: "Beta", Type: EntryDirectory},
		{ID: "f-a", Path: "a.txt", Name: "a.txt", Type: EntryFile},
		{ID: "d-a", Path: "alpha", Name: "alpha", Type: EntryDirectory},
	}
	model := newModel(entries)

	got := make([]string, 0, len(model.entries))
	for _, entry := range model.entries {
		got = append(got, entry.ID)
	}
	want := []string{"d-a", "d-b", "f-a", "f-z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted IDs = %v, want %v", got, want)
	}

	for range 3 {
		model.handle(keyDown, 2)
	}
	if model.cursor != 3 || model.offset != 2 {
		t.Fatalf("cursor/offset = %d/%d, want 3/2", model.cursor, model.offset)
	}
	model.handle(keyUp, 2)
	model.handle(keyUp, 2)
	if model.cursor != 1 || model.offset != 1 {
		t.Fatalf("cursor/offset after moving up = %d/%d, want 1/1", model.cursor, model.offset)
	}
}

func TestSpaceOnlySelectsFiles(t *testing.T) {
	model := newModel([]Entry{
		{ID: "dir", Path: "folder", Name: "folder", Type: EntryDirectory},
		{ID: "file", Path: "file.txt", Name: "file.txt", Type: EntryFile},
	})

	model.handle(keySpace, 10)
	if len(model.selected) != 0 {
		t.Fatalf("selected directory; selected = %#v", model.selected)
	}
	model.handle(keyDown, 10)
	model.handle(keySpace, 10)
	if len(model.selected) != 1 {
		t.Fatalf("selected count = %d, want 1", len(model.selected))
	}
	model.handle(keySpace, 10)
	if len(model.selected) != 0 {
		t.Fatalf("second toggle did not clear selection: %#v", model.selected)
	}
}

func TestNavigationRestoresParentPosition(t *testing.T) {
	model := newModel([]Entry{
		{ID: "one", Path: "one", Name: "one", Type: EntryDirectory},
		{ID: "two", Path: "two", Name: "two", Type: EntryDirectory},
	})
	model.handle(keyDown, 1)
	effect := model.handle(keyRight, 1)
	if effect.kind != effectEnterDirectory || effect.entry.ID != "two" {
		t.Fatalf("enter effect = %#v", effect)
	}
	model.enter(effect.entry.Path, nil)
	model.leave()
	if model.dir != "" || model.cursor != 1 || model.offset != 1 {
		t.Fatalf("restored location = dir %q cursor %d offset %d", model.dir, model.cursor, model.offset)
	}
}
