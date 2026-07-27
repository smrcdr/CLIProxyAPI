package smartrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileRouterAuditSink appends one masked JSON event per line.
type FileRouterAuditSink struct {
	path string
	mu   sync.Mutex
}

func NewFileRouterAuditSink(path string) (*FileRouterAuditSink, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("create router audit sink: path is empty")
	}
	return &FileRouterAuditSink{path: filepath.Clean(path)}, nil
}

func (s *FileRouterAuditSink) WriteRouterAudit(ctx context.Context, event RouterAuditEvent) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if s == nil {
		return errors.New("write router audit: sink is nil")
	}
	event.Action = strings.TrimSpace(event.Action)
	event.ResourceType = strings.TrimSpace(event.ResourceType)
	event.ResourceID = strings.TrimSpace(event.ResourceID)
	event.Actor = strings.TrimSpace(event.Actor)
	event.Outcome = strings.TrimSpace(event.Outcome)
	event.FailureCategory = strings.TrimSpace(event.FailureCategory)
	if event.Action == "" || event.ResourceType == "" || event.Outcome == "" {
		return errors.New("write router audit: action, resource type, and outcome are required")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	event.ChangedFields = append([]string(nil), event.ChangedFields...)

	data, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		return fmt.Errorf("marshal router audit event: %w", errMarshal)
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if errMkdir := os.MkdirAll(filepath.Dir(s.path), routerStorageDirMode); errMkdir != nil {
		return fmt.Errorf("create router audit directory: %w", errMkdir)
	}
	file, errOpen := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, routerMetadataFileMode)
	if errOpen != nil {
		return fmt.Errorf("open router audit file: %w", errOpen)
	}
	defer func() {
		_ = file.Close()
	}()
	if _, errWrite := file.Write(data); errWrite != nil {
		return fmt.Errorf("append router audit event: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		return fmt.Errorf("sync router audit event: %w", errSync)
	}
	return nil
}
