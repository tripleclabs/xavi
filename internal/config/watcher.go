package config

import (
	"context"
	"log"
	"os"
	"time"
)

// Watcher watches a config file for changes.
type Watcher struct {
	Path     string
	Interval time.Duration
	Updates  chan *Config
}

// NewWatcher creates a new config watcher.
func NewWatcher(path string, interval time.Duration) *Watcher {
	return &Watcher{
		Path:     path,
		Interval: interval,
		Updates:  make(chan *Config, 1),
	}
}

// Run starts the watcher loop.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	var lastModTime time.Time

	// Initial check
	if info, err := os.Stat(w.Path); err == nil {
		lastModTime = info.ModTime()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(w.Path)
			if err != nil {
				// File might not exist yet or was deleted, ignore
				continue
			}

			if info.ModTime().After(lastModTime) {
				lastModTime = info.ModTime()
				log.Printf("Config file %s changed, reloading...", w.Path)
				cfg, err := LoadFromFile(w.Path)
				if err != nil {
					log.Printf("Failed to reload config: %v", err)
					continue
				}

				// Non-blocking send
				select {
				case w.Updates <- cfg:
				default:
				}
			}
		}
	}
}
