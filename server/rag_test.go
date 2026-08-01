package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRAGStoreSplitsMarkdownAndBracketSections(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "campus.md"), []byte(`# The Hub
The Hub is a first contact point for student questions.

## FoSE
The Faculty of Science and Engineering supports engineering and science students.
`), 0644); err != nil {
		t.Fatalf("write markdown: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "support.txt"), []byte(`[Emergency]
For urgent danger, contact professional or emergency support immediately.
`), 0644); err != nil {
		t.Fatalf("write text: %v", err)
	}

	store, err := loadRAGStore(dir)
	if err != nil {
		t.Fatalf("loadRAGStore returned error: %v", err)
	}

	files, sections := store.Stats()
	if files != 2 || sections != 3 {
		t.Fatalf("stats = files %d sections %d, want 2/3", files, sections)
	}
}

func TestLoadRAGStoreAllowsMissingDirectory(t *testing.T) {
	store, err := loadRAGStore(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("loadRAGStore returned error: %v", err)
	}

	files, sections := store.Stats()
	if files != 0 || sections != 0 {
		t.Fatalf("stats = files %d sections %d, want 0/0", files, sections)
	}
	result := store.Retrieve("FoSE support", 4, 1000, 0.01)
	if result.Route != ragRouteNoMatch {
		t.Fatalf("route = %q, want %q", result.Route, ragRouteNoMatch)
	}
}

func TestRAGRetrieveFindsRelevantUNNCSection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "knowledge.md"), []byte(`# BlueSee
BlueSee is the mascot and AI assistant for FoSE at University of Nottingham Ningbo China.

## Catering
Campus dining information should be checked with official campus services.
`), 0644); err != nil {
		t.Fatalf("write knowledge: %v", err)
	}
	store, err := loadRAGStore(dir)
	if err != nil {
		t.Fatalf("loadRAGStore returned error: %v", err)
	}

	result := store.Retrieve("Who is BlueSee for FoSE UNNC?", 1, 1000, 0.01)
	if result.Route != ragRouteContext {
		t.Fatalf("route = %q, want %q", result.Route, ragRouteContext)
	}
	if !strings.Contains(result.Context, "mascot and AI assistant") {
		t.Fatalf("context missing expected section: %q", result.Context)
	}
	if len(result.Sections) != 1 {
		t.Fatalf("sections len = %d, want 1", len(result.Sections))
	}
}

func TestRAGRetrieveReturnsNoMatchForUnrelatedQuery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "knowledge.md"), []byte(`# BlueSee
BlueSee helps FoSE students and staff with campus-oriented questions.
`), 0644); err != nil {
		t.Fatalf("write knowledge: %v", err)
	}
	store, err := loadRAGStore(dir)
	if err != nil {
		t.Fatalf("loadRAGStore returned error: %v", err)
	}

	result := store.Retrieve("banana rocket astronomy", 3, 1000, 0.2)
	if result.Route != ragRouteNoMatch {
		t.Fatalf("route = %q, want %q", result.Route, ragRouteNoMatch)
	}
	if result.Context != "" {
		t.Fatalf("context = %q, want empty", result.Context)
	}
}

func TestRAGRetrieveHonorsTopKAndMaxContextChars(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "knowledge.md"), []byte(`# FoSE One
FoSE support information for students includes first contact guidance and admin caveats.

## FoSE Two
FoSE support information repeats so this section can also match the same query.

## FoSE Three
FoSE support information repeats again for ranking and topK behavior.
`), 0644); err != nil {
		t.Fatalf("write knowledge: %v", err)
	}
	store, err := loadRAGStore(dir)
	if err != nil {
		t.Fatalf("loadRAGStore returned error: %v", err)
	}

	result := store.Retrieve("FoSE support information", 2, 180, 0.01)
	if result.Route != ragRouteContext {
		t.Fatalf("route = %q, want %q", result.Route, ragRouteContext)
	}
	if len(result.Sections) > 2 {
		t.Fatalf("sections len = %d, want at most 2", len(result.Sections))
	}
	if result.ContextChars > 180 {
		t.Fatalf("context chars = %d, want <= 180", result.ContextChars)
	}
}
