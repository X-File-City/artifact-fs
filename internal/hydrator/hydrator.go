package hydrator

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type BlobFetcher interface {
	BlobToCache(ctx context.Context, repo model.RepoConfig, objectOID string, dstPath string) (size int64, err error)
}

// OnHydratedFunc is called after a blob is successfully fetched. Allows the
// caller to update metadata (e.g., backfill file sizes in the snapshot).
type OnHydratedFunc func(repoID model.RepoID, objectOID string, size int64)

type Service struct {
	fetcher    BlobFetcher
	mu         sync.Mutex
	pq         priorityQueue
	wait       map[string][]chan result
	started    bool
	stopOnce   sync.Once
	stopCh     chan struct{}
	workReady  chan struct{} // signaled when new work is enqueued
	onHydrated OnHydratedFunc
}

type result struct {
	cachePath string
	size      int64
	err       error
}

func New(fetcher BlobFetcher) *Service {
	return &Service{
		fetcher:   fetcher,
		wait:      map[string][]chan result{},
		stopCh:    make(chan struct{}),
		workReady: make(chan struct{}, 1),
	}
}

// SetOnHydrated registers a callback invoked after each successful blob fetch.
func (s *Service) SetOnHydrated(fn OnHydratedFunc) {
	s.mu.Lock()
	s.onHydrated = fn
	s.mu.Unlock()
}

// signalWork performs a non-blocking send on workReady to wake a worker.
func (s *Service) signalWork() {
	select {
	case s.workReady <- struct{}{}:
	default: // already signaled, workers will drain the queue
	}
}

func (s *Service) Start(workers int, repo model.RepoConfig) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	for i := 0; i < workers; i++ {
		go s.worker(repo)
	}
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		// Drain pending waiters so they don't block forever
		s.mu.Lock()
		defer s.mu.Unlock()
		for key, chans := range s.wait {
			for _, ch := range chans {
				ch <- result{err: errors.New("hydrator stopped")}
				close(ch)
			}
			delete(s.wait, key)
		}
	})
}

func (s *Service) Enqueue(task model.HydrationTask) {
	s.mu.Lock()
	heap.Push(&s.pq, &taskItem{task: task})
	s.mu.Unlock()
	s.signalWork()
}

func (s *Service) EnsureHydrated(ctx context.Context, repo model.RepoConfig, path string, oid string) (cachePath string, size int64, err error) {
	cachePath = filepath.Join(repo.BlobCacheDir, oid)
	if st, err := os.Stat(cachePath); err == nil {
		return cachePath, st.Size(), nil
	}
	key := string(repo.ID) + ":" + oid
	ch := make(chan result, 1)

	s.mu.Lock()
	s.wait[key] = append(s.wait[key], ch)
	first := len(s.wait[key]) == 1
	if first {
		heap.Push(&s.pq, &taskItem{task: model.HydrationTask{RepoID: repo.ID, Path: path, ObjectOID: oid, Priority: PriorityExplicitRead, Reason: "explicit read", EnqueuedAt: time.Now()}})
	}
	s.mu.Unlock()
	if first {
		s.signalWork()
	}

	select {
	case <-ctx.Done():
		// Remove our channel from the wait list so the worker doesn't
		// send to an abandoned channel that nobody reads.
		s.mu.Lock()
		if chans, ok := s.wait[key]; ok {
			for i, c := range chans {
				if c == ch {
					s.wait[key] = append(chans[:i], chans[i+1:]...)
					break
				}
			}
		}
		s.mu.Unlock()
		return "", 0, ctx.Err()
	case r := <-ch:
		return r.cachePath, r.size, r.err
	}
}

func (s *Service) QueueDepth(repoID model.RepoID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for _, item := range s.pq {
		if item.task.RepoID == repoID {
			c++
		}
	}
	return c
}

func (s *Service) worker(repo model.RepoConfig) {
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.workReady:
			// Drain the queue: process all available items before waiting
			// for the next signal. Re-signal if items remain so other
			// workers can help.
			for {
				if !s.step(repo) {
					break
				}
				s.signalWork()
			}
		}
	}
}

// step pops and processes one item from the queue. Returns true if an item
// was processed, false if the queue was empty.
func (s *Service) step(repo model.RepoConfig) bool {
	s.mu.Lock()
	if len(s.pq) == 0 {
		s.mu.Unlock()
		return false
	}
	item := heap.Pop(&s.pq).(*taskItem)
	key := string(item.task.RepoID) + ":" + item.task.ObjectOID
	waits := s.wait[key]
	delete(s.wait, key)
	s.mu.Unlock()

	cachePath := filepath.Join(repo.BlobCacheDir, item.task.ObjectOID)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		s.notify(waits, result{err: err})
		return true
	}
	// Use a timeout context derived from stopCh so stuck blob fetches don't
	// block a worker forever.
	fetchCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-fetchCtx.Done():
		}
	}()
	size, err := s.fetcher.BlobToCache(fetchCtx, repo, item.task.ObjectOID, cachePath)
	if err != nil {
		s.notify(waits, result{err: fmt.Errorf("hydrate %s: %w", item.task.Path, err)})
		return true
	}
	s.notify(waits, result{cachePath: cachePath, size: size, err: nil})
	s.mu.Lock()
	fn := s.onHydrated
	s.mu.Unlock()
	if fn != nil {
		fn(item.task.RepoID, item.task.ObjectOID, size)
	}
	return true
}

func (s *Service) notify(chans []chan result, r result) {
	for _, ch := range chans {
		ch <- r
		close(ch)
	}
}

const (
	PriorityExplicitRead = 1000
	PrioritySibling      = 800
	PriorityBootstrap    = 700
	PriorityLikelyText   = 500
	PriorityNearbyCode   = 400
	PriorityBinary       = 100
)

func ClassifyPriority(path string) int {
	base := filepath.Base(path)
	ext := filepath.Ext(path)
	switch {
	case base == "README" || base == "README.md" || base == "LICENSE" || base == "Makefile" || base == ".gitignore":
		return PriorityBootstrap
	case base == "go.mod" || base == "go.sum" || base == "Cargo.toml" || base == "package.json" || base == "pnpm-lock.yaml" || base == "pyproject.toml":
		return PriorityBootstrap
	case isCodeExtension(ext):
		return PriorityLikelyText
	case ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".zip" || ext == ".pdf" || ext == ".tar" || ext == ".gz" || ext == ".mp4" || ext == ".mov" || ext == ".avi":
		return PriorityBinary
	default:
		return PriorityNearbyCode
	}
}

func isCodeExtension(ext string) bool {
	switch ext {
	case ".go", ".rs", ".zig", ".py", ".ts", ".tsx", ".js", ".jsx",
		".java", ".c", ".cc", ".cpp", ".h", ".hpp",
		".json", ".yaml", ".yml", ".toml", ".md":
		return true
	}
	return false
}

type taskItem struct {
	task  model.HydrationTask
	index int
}

type priorityQueue []*taskItem

func (p priorityQueue) Len() int { return len(p) }
func (p priorityQueue) Less(i, j int) bool {
	if p[i].task.Priority == p[j].task.Priority {
		return p[i].task.EnqueuedAt.Before(p[j].task.EnqueuedAt)
	}
	return p[i].task.Priority > p[j].task.Priority
}
func (p priorityQueue) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
	p[i].index, p[j].index = i, j
}
func (p *priorityQueue) Push(x any) {
	item := x.(*taskItem)
	item.index = len(*p)
	*p = append(*p, item)
}
func (p *priorityQueue) Pop() any {
	old := *p
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*p = old[:n-1]
	return item
}
