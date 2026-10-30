//go:build !linux

package watcher

import (
	"fmt"

	"github.com/colthreepv/immich-optimizer/internal/domain/port"
	"github.com/colthreepv/immich-optimizer/internal/logger"
)

func NewInotifyWatcher(watchDir string, log *logger.Logger, bufferSize int) (port.FileWatcher, error) {
	return nil, fmt.Errorf("inotify watcher is only supported on Linux")
}
