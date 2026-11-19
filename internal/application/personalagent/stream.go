package personalagent

import (
	"context"
	"encoding/json"

	"agentflow/internal/domain"
)

type taskWatchKey struct {
	projectID string
	actorID   string
	taskID    string
}

type taskWatcher struct {
	updates      chan domain.PersonalAgentTask
	done         chan struct{}
	lastRevision int
}

// Watch atomically loads the caller's current task snapshot and subscribes to
// subsequent successful in-process persistence updates. It never polls storage.
// Reconnects always get the latest complete snapshot; Revision is its cursor.
// Slow readers may skip intermediate snapshots, but retain the newest state.
// Cancellation ends only this subscription, not the background task.
func (s *Service) Watch(ctx context.Context, scope Scope, id string) (initial domain.PersonalAgentTask, updates <-chan domain.PersonalAgentTask, cancel func(), err error) {
	if err := scopeValid(scope); err != nil {
		return initial, nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return initial, nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return initial, nil, nil, ErrConflict
	}
	initial, err = s.store.PersonalAgentTask(scope.ProjectID, scope.ActorID, id)
	if err != nil {
		return initial, nil, nil, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return domain.PersonalAgentTask{}, nil, nil, err
	}
	key := taskWatchKey{projectID: scope.ProjectID, actorID: scope.ActorID, taskID: id}
	watcher := &taskWatcher{updates: make(chan domain.PersonalAgentTask, 1), done: make(chan struct{}), lastRevision: initial.Revision}
	if s.watchers[key] == nil {
		s.watchers[key] = map[*taskWatcher]struct{}{}
	}
	s.watchers[key][watcher] = struct{}{}
	cancel = func() {
		s.mu.Lock()
		s.removeWatcherLocked(key, watcher)
		s.mu.Unlock()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			cancel()
		case <-watcher.done:
		}
	}()
	return initial, watcher.updates, cancel, nil
}

// publishLocked is called only after persistence succeeds, under the same lock
// used by Watch's snapshot+registration. Buffered snapshots are owned by their
// recipient: copying the nested JSON data avoids sharing mutable slices/maps
// with other subscribers or a mutation method's returned task.
func (s *Service) publishLocked(task domain.PersonalAgentTask) {
	key := taskWatchKey{projectID: task.ProjectID, actorID: task.ActorID, taskID: task.ID}
	watchers := s.watchers[key]
	if len(watchers) == 0 {
		return
	}
	raw, err := json.Marshal(task)
	if err != nil {
		// A persisted task must be JSON serializable. If a nonstandard Store
		// violates that contract, end subscribers rather than emit bad state.
		for watcher := range watchers {
			s.removeWatcherLocked(key, watcher)
		}
		return
	}
	for watcher := range watchers {
		if task.Revision <= watcher.lastRevision {
			continue
		}
		var snapshot domain.PersonalAgentTask
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			s.removeWatcherLocked(key, watcher)
			continue
		}
		watcher.lastRevision = task.Revision
		select {
		case watcher.updates <- snapshot:
		default:
			// Only publishers holding s.mu can send or close this channel.
			// A reader may drain it concurrently, so the drain is nonblocking.
			select {
			case <-watcher.updates:
			default:
			}
			select {
			case watcher.updates <- snapshot:
			default:
			}
		}
	}
}

func (s *Service) removeWatcherLocked(key taskWatchKey, watcher *taskWatcher) {
	if _, ok := s.watchers[key][watcher]; !ok {
		return
	}
	delete(s.watchers[key], watcher)
	if len(s.watchers[key]) == 0 {
		delete(s.watchers, key)
	}
	close(watcher.updates)
	close(watcher.done)
}

func (s *Service) closeWatchersLocked() {
	for key, watchers := range s.watchers {
		for watcher := range watchers {
			s.removeWatcherLocked(key, watcher)
		}
	}
}
