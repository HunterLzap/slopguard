package ui

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	previewTTL          = 10 * time.Minute
	maxPendingPreviews  = 128
	previewIDLengthHint = 64
)

var (
	ErrPreviewRequired       = errors.New("配置修改必须先预览 Diff（缺少 preview_id）")
	ErrPreviewExpired        = errors.New("预览已过期，请重新预览 Diff")
	ErrPreviewUsed           = errors.New("预览已被使用，请重新预览 Diff")
	ErrPreviewScopeMismatch  = errors.New("预览记录与当前操作的项目、目标或操作类型不一致")
	ErrPreviewConflict       = errors.New("目标文件在预览后已被外部修改，请重新预览 Diff")
	ErrPreviewContentChanged = errors.New("预览内容已变化，请重新预览 Diff")
)

type previewRecord struct {
	ID        string
	Kind      string
	TargetID  string
	Project   string
	Scope     string
	Action    string
	Paths     map[string]string
	NewHashes map[string]string
	CreatedAt time.Time
	ExpiresAt time.Time
	Consumed  bool
}

type previewExpectation struct {
	Kind           string
	TargetID       string
	Project        string
	Scope          string
	Action         string
	Paths          []string
	ExpectedHashes map[string]string
}

type applyLease struct {
	server *Server
	record *previewRecord
	once   sync.Once
}

func (l *applyLease) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.server != nil {
			l.server.mutationMu.Unlock()
		}
	})
}

func normalizePreviewProject(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if abs, err := filepath.Abs(raw); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(raw)
}

func normalizePreviewPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

func (s *Server) createPreview(rec previewRecord) (previewRecord, error) {
	if s == nil {
		return previewRecord{}, errors.New("ui: nil server")
	}
	if len(rec.Paths) == 0 {
		return previewRecord{}, errors.New("ui: preview has no target paths")
	}
	normalizedPaths := make(map[string]string, len(rec.Paths))
	for path, hash := range rec.Paths {
		clean := normalizePreviewPath(path)
		if clean == "" {
			return previewRecord{}, errors.New("ui: preview contains an empty target path")
		}
		normalizedPaths[clean] = hash
	}
	normalizedNew := make(map[string]string, len(rec.NewHashes))
	for path, hash := range rec.NewHashes {
		clean := normalizePreviewPath(path)
		if clean != "" {
			normalizedNew[clean] = hash
		}
	}
	now := time.Now()
	rec.ID = generateRandomToken()
	rec.Paths = normalizedPaths
	rec.NewHashes = normalizedNew
	rec.Project = normalizePreviewProject(rec.Project)
	rec.Kind = strings.TrimSpace(rec.Kind)
	rec.TargetID = strings.TrimSpace(rec.TargetID)
	rec.Scope = strings.TrimSpace(rec.Scope)
	rec.Action = strings.ToLower(strings.TrimSpace(rec.Action))
	rec.CreatedAt = now
	rec.ExpiresAt = now.Add(previewTTL)
	rec.Consumed = false

	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	if s.previews == nil {
		s.previews = make(map[string]*previewRecord)
	}
	s.cleanupPreviewsLocked(now)
	copy := rec
	s.previews[rec.ID] = &copy
	return rec, nil
}

func (s *Server) cleanupPreviewsLocked(now time.Time) {
	for id, rec := range s.previews {
		if rec == nil || !now.Before(rec.ExpiresAt) || rec.Consumed {
			delete(s.previews, id)
		}
	}
	if len(s.previews) <= maxPendingPreviews {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, rec := range s.previews {
		if rec == nil {
			continue
		}
		if oldestID == "" || rec.CreatedAt.Before(oldest) {
			oldestID = id
			oldest = rec.CreatedAt
		}
	}
	if oldestID != "" {
		delete(s.previews, oldestID)
	}
}

func (s *Server) beginApply(previewID string, expect previewExpectation) (*applyLease, error) {
	if s == nil {
		return nil, errors.New("ui: nil server")
	}
	previewID = strings.TrimSpace(previewID)
	if previewID == "" {
		return nil, ErrPreviewRequired
	}

	// Serialize all config mutations and keep the lock until the caller has
	// finished writing and updating backup metadata.
	s.mutationMu.Lock()
	unlock := func() { s.mutationMu.Unlock() }

	now := time.Now()
	s.previewMu.Lock()
	rec := s.previews[previewID]
	if rec == nil {
		s.previewMu.Unlock()
		unlock()
		return nil, ErrPreviewExpired
	}
	if rec.Consumed {
		s.previewMu.Unlock()
		unlock()
		return nil, ErrPreviewUsed
	}
	if !now.Before(rec.ExpiresAt) {
		delete(s.previews, previewID)
		s.previewMu.Unlock()
		unlock()
		return nil, ErrPreviewExpired
	}
	if !previewMatchesExpectation(*rec, expect) {
		s.previewMu.Unlock()
		unlock()
		return nil, ErrPreviewScopeMismatch
	}

	record := *rec
	record.Paths = cloneStringMap(rec.Paths)
	record.NewHashes = cloneStringMap(rec.NewHashes)
	s.previewMu.Unlock()

	for path, baseHash := range record.Paths {
		currentHash, existed, err := currentFileSHA256(path)
		if err != nil {
			unlock()
			return nil, fmt.Errorf("检查目标文件版本失败 (%s): %w", path, err)
		}
		if !existed {
			currentHash = ""
		}
		if currentHash != baseHash {
			unlock()
			return nil, fmt.Errorf("%w: %s", ErrPreviewConflict, path)
		}
	}
	for path, expectedHash := range expect.ExpectedHashes {
		clean := normalizePreviewPath(path)
		if clean == "" {
			continue
		}
		baseHash, ok := record.Paths[clean]
		if !ok {
			unlock()
			return nil, fmt.Errorf("%w: 未知目标路径 %s", ErrPreviewScopeMismatch, clean)
		}
		if strings.TrimSpace(expectedHash) != baseHash {
			unlock()
			return nil, fmt.Errorf("%w: %s", ErrPreviewConflict, clean)
		}
	}

	s.previewMu.Lock()
	if live := s.previews[previewID]; live != nil {
		live.Consumed = true
	}
	s.previewMu.Unlock()

	return &applyLease{server: s, record: &record}, nil
}

func previewMatchesExpectation(rec previewRecord, expect previewExpectation) bool {
	if strings.TrimSpace(rec.Kind) != strings.TrimSpace(expect.Kind) {
		return false
	}
	if strings.TrimSpace(rec.TargetID) != strings.TrimSpace(expect.TargetID) {
		return false
	}
	if rec.Action != strings.ToLower(strings.TrimSpace(expect.Action)) {
		return false
	}
	if normalizePreviewProject(rec.Project) != normalizePreviewProject(expect.Project) {
		return false
	}
	if strings.TrimSpace(expect.Scope) != "" && rec.Scope != strings.TrimSpace(expect.Scope) {
		return false
	}
	if len(expect.Paths) > 0 {
		if len(expect.Paths) != len(rec.Paths) {
			return false
		}
		for _, path := range expect.Paths {
			clean := normalizePreviewPath(path)
			if _, ok := rec.Paths[clean]; !ok {
				return false
			}
		}
	}
	return true
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func (l *applyLease) checkNewContent(path string, data []byte) error {
	if l == nil || l.record == nil {
		return errors.New("ui: missing preview lease")
	}
	clean := normalizePreviewPath(path)
	expected, ok := l.record.NewHashes[clean]
	if !ok {
		if _, known := l.record.Paths[clean]; known {
			return fmt.Errorf("%w: %s", ErrPreviewContentChanged, clean)
		}
		return fmt.Errorf("%w: 未知目标路径 %s", ErrPreviewScopeMismatch, clean)
	}
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("%w: %s", ErrPreviewContentChanged, clean)
	}
	if fileSHA256Hex(data) != expected {
		return fmt.Errorf("%w: %s", ErrPreviewContentChanged, clean)
	}
	return nil
}

// checkNewHash verifies a precomputed final-content hash for a target before
// performing multi-target writes. It never allows a path outside the preview.
func (l *applyLease) checkNewHash(path, hash string) error {
	if l == nil || l.record == nil {
		return errors.New("ui: missing preview lease")
	}
	clean := normalizePreviewPath(path)
	expected, ok := l.record.NewHashes[clean]
	if !ok {
		if _, known := l.record.Paths[clean]; known {
			return fmt.Errorf("%w: %s", ErrPreviewContentChanged, clean)
		}
		return fmt.Errorf("%w: 未知目标路径 %s", ErrPreviewScopeMismatch, clean)
	}
	if strings.TrimSpace(expected) == "" || strings.TrimSpace(hash) == "" {
		return fmt.Errorf("%w: %s", ErrPreviewContentChanged, clean)
	}
	if hash != expected {
		return fmt.Errorf("%w: %s", ErrPreviewContentChanged, clean)
	}
	return nil
}

func (l *applyLease) checkDeletion(path string) error {
	if l == nil || l.record == nil {
		return errors.New("ui: missing preview lease")
	}
	clean := normalizePreviewPath(path)
	if _, known := l.record.Paths[clean]; !known {
		return fmt.Errorf("%w: 未知目标路径 %s", ErrPreviewScopeMismatch, clean)
	}
	expected, ok := l.record.NewHashes[clean]
	if !ok {
		return fmt.Errorf("%w: missing deletion preview for %s", ErrPreviewScopeMismatch, clean)
	}
	if strings.TrimSpace(expected) != "" {
		return fmt.Errorf("%w: %s", ErrPreviewContentChanged, clean)
	}
	return nil
}
