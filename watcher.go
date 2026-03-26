package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// poolWatcher watches the pool directory and config file for changes,
// triggering automatic reloads without requiring a server restart.
// State directory changes are ignored to avoid reload loops from self-writes.
type poolWatcher struct {
	watcher    *fsnotify.Watcher
	poolDir    string
	stateDir   string // state directory (ignored for reload triggers)
	configPath string
	handler    *proxyHandler

	mu           sync.Mutex
	debouncePool *time.Timer
	debounceCfg  *time.Timer
}

const watcherDebounce = 500 * time.Millisecond

func newPoolWatcher(poolDir, stateDir, configPath string, handler *proxyHandler) (*poolWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	pw := &poolWatcher{
		watcher:    w,
		poolDir:    poolDir,
		stateDir:   stateDir,
		configPath: configPath,
		handler:    handler,
	}

	// Watch pool directory for credential file changes.
	if poolDir != "" {
		count, err := addDirectoryTreeWatch(w, poolDir)
		if err != nil {
			w.Close()
			return nil, err
		}
		log.Printf("watching pool directory tree: %s (%d dirs)", poolDir, count)
	}

	// Watch state directory so fsnotify doesn't miss creates, but events from it
	// are ignored in handleEvent to avoid reload loops from codex-pool's own writes.
	if stateDir != "" {
		count, err := addDirectoryTreeWatch(w, stateDir)
		if err != nil {
			// Non-fatal: state dir may not exist yet (created on first save).
			log.Printf("warning: cannot watch state directory %s: %v (will be created on first save)", stateDir, err)
		} else if count > 0 {
			log.Printf("watching state directory tree (ignored for reloads): %s (%d dirs)", stateDir, count)
		}
	}

	// Watch config file for setting changes.
	if configPath != "" {
		if err := w.Add(configPath); err != nil {
			// Non-fatal — config may not exist yet.
			log.Printf("warning: cannot watch config file %s: %v", configPath, err)
		} else {
			log.Printf("watching config file: %s", configPath)
		}
	}

	go pw.loop()
	return pw, nil
}

func (pw *poolWatcher) loop() {
	for {
		select {
		case event, ok := <-pw.watcher.Events:
			if !ok {
				return
			}
			// Ignore chmod-only events.
			if event.Op == fsnotify.Chmod {
				continue
			}
			pw.handleEvent(event)

		case err, ok := <-pw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
		}
	}
}

func (pw *poolWatcher) handleEvent(event fsnotify.Event) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	// Is this the config file?
	if pw.configPath != "" && event.Name == pw.configPath {
		if pw.debounceCfg != nil {
			pw.debounceCfg.Stop()
		}
		pw.debounceCfg = time.AfterFunc(watcherDebounce, pw.reloadConfig)
		return
	}

	// Ignore events from the state directory. State changes are initiated by
	// codex-pool itself (saves after refresh); reloading on self-writes would
	// cause a reload loop. External edits to state files require touching a
	// seed file or restarting to take effect.
	if pw.stateDir != "" {
		absState, _ := filepath.Abs(pw.stateDir)
		absEvent, _ := filepath.Abs(event.Name)
		if absState != "" && absEvent != "" && strings.HasPrefix(absEvent, absState+string(filepath.Separator)) {
			return
		}
	}

	if event.Has(fsnotify.Create) {
		count, err := addDirectoryTreeWatch(pw.watcher, event.Name)
		if err != nil {
			log.Printf("warning: failed to watch new pool path %s: %v", event.Name, err)
		} else if count > 0 {
			log.Printf("watching new pool directory tree: %s (%d dirs)", event.Name, count)
		}
	}

	// Otherwise it's a pool directory change.
	if pw.debouncePool != nil {
		pw.debouncePool.Stop()
	}
	pw.debouncePool = time.AfterFunc(watcherDebounce, pw.reloadPool)
}

func (pw *poolWatcher) reloadPool() {
	log.Printf("pool directory changed, reloading accounts")
	pw.handler.reloadAccounts()
	counts := map[AccountType]int{}
	pw.handler.pool.mu.RLock()
	for _, a := range pw.handler.pool.accounts {
		counts[a.Type]++
	}
	pw.handler.pool.mu.RUnlock()
	log.Printf("hot-reload complete: codex=%d claude=%d gemini=%d kimi=%d minimax=%d",
		counts[AccountTypeCodex], counts[AccountTypeClaude], counts[AccountTypeGemini],
		counts[AccountTypeKimi], counts[AccountTypeMinimax])
}

func (pw *poolWatcher) reloadConfig() {
	log.Printf("config file changed, reloading non-sensitive settings")
	cfg, err := loadConfigFile(pw.configPath)
	if err != nil {
		log.Printf("config reload failed: %v", err)
		return
	}
	if cfg == nil {
		return
	}

	// Only reload safe, non-sensitive fields.
	newDebug := getConfigBool("DEBUG", cfg.Debug, false)
	pw.handler.cfg.debug.Store(newDebug)
	pw.handler.cfg.tierThreshold = getConfigFloat64("TIER_THRESHOLD", cfg.TierThreshold, 0.15)
	pw.handler.pool.mu.Lock()
	pw.handler.pool.debug = newDebug
	pw.handler.pool.mu.Unlock()

	// Reload model aliases.
	if pw.handler.aliases != nil && cfg.ModelAliases != nil {
		pw.handler.aliases.reload(cfg.ModelAliases)
		log.Printf("reloaded %d model aliases", len(cfg.ModelAliases))
	}

	log.Printf("config hot-reload complete (debug=%v, tier_threshold=%.2f)",
		newDebug, pw.handler.cfg.tierThreshold)
}

func (pw *poolWatcher) close() {
	pw.watcher.Close()
}

func addDirectoryTreeWatch(w *fsnotify.Watcher, root string) (int, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, nil
	}

	dirs, err := collectWatchDirs(root)
	if err != nil {
		return 0, err
	}

	for _, dir := range dirs {
		if err := w.Add(dir); err != nil {
			return 0, err
		}
	}

	return len(dirs), nil
}

func collectWatchDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dirs, nil
}
