package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestCollectWatchDirsIncludesNestedDirectories(t *testing.T) {
	root := t.TempDir()
	want := []string{
		root,
		filepath.Join(root, "claude"),
		filepath.Join(root, "codex"),
		filepath.Join(root, "gemini"),
		filepath.Join(root, "gemini", "nested"),
	}

	for _, dir := range want[1:] {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	got, err := collectWatchDirs(root)
	if err != nil {
		t.Fatalf("collectWatchDirs: %v", err)
	}

	gotSet := make(map[string]bool, len(got))
	for _, dir := range got {
		gotSet[dir] = true
	}

	for _, dir := range want {
		if !gotSet[dir] {
			t.Fatalf("expected %s in watched directory tree, got %v", dir, got)
		}
	}
}

func TestAddDirectoryTreeWatchAddsNestedDirectories(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, "codex"),
		filepath.Join(root, "claude"),
		filepath.Join(root, "gemini", "nested"),
	}
	for _, dir := range paths {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	count, err := addDirectoryTreeWatch(w, root)
	if err != nil {
		t.Fatalf("addDirectoryTreeWatch: %v", err)
	}

	watchSet := make(map[string]bool, len(w.WatchList()))
	for _, dir := range w.WatchList() {
		watchSet[dir] = true
	}

	want := []string{
		root,
		filepath.Join(root, "codex"),
		filepath.Join(root, "claude"),
		filepath.Join(root, "gemini"),
		filepath.Join(root, "gemini", "nested"),
	}
	if count != len(want) {
		t.Fatalf("watch count = %d, want %d", count, len(want))
	}
	for _, dir := range want {
		if !watchSet[dir] {
			t.Fatalf("expected watcher to include %s, got %v", dir, w.WatchList())
		}
	}
}
