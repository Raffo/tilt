package k8swatch

import (
	"context"
	"sync"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/windmilleng/tilt/internal/k8s"
	"github.com/windmilleng/tilt/internal/store"
	"github.com/windmilleng/tilt/pkg/logger"
	"github.com/windmilleng/tilt/pkg/model"
)

type JobWatcher struct {
	kCli         k8s.Client
	ownerFetcher k8s.OwnerFetcher
	watching     bool
	nodeIP       k8s.NodeIP

	mu                sync.RWMutex
	knownDeployedUIDs map[types.UID]model.ManifestName
	knownJobs         map[types.UID]*batchv1.Job
}

func NewJobWatcher(kCli k8s.Client, ownerFetcher k8s.OwnerFetcher, nodeIP k8s.NodeIP) *JobWatcher {
	return &JobWatcher{
		kCli:              kCli,
		ownerFetcher:      ownerFetcher,
		nodeIP:            nodeIP,
		knownDeployedUIDs: make(map[types.UID]model.ManifestName),
		knownJobs:         make(map[types.UID]*batchv1.Job),
	}
}

func (w *JobWatcher) diff(st store.RStore) watcherTaskList {
	state := st.RLockState()
	defer st.RUnlockState()

	w.mu.RLock()
	defer w.mu.RUnlock()

	taskList := createWatcherTaskList(state, w.knownDeployedUIDs)
	if w.watching {
		taskList.needsWatch = false
	}
	return taskList
}

func (w *JobWatcher) OnChange(ctx context.Context, st store.RStore) {
	taskList := w.diff(st)
	if taskList.needsWatch {
		w.setupWatch(ctx, st)
	}

	if len(taskList.newUIDs) > 0 {
		w.setupNewUIDs(ctx, st, taskList.newUIDs)
	}
}

func (w *JobWatcher) setupWatch(ctx context.Context, st store.RStore) {
	w.watching = true

	ch, err := w.kCli.WatchJobs(ctx, k8s.ManagedByTiltSelector())
	if err != nil {
		err = errors.Wrap(err, "Error watching Jobs. Are you connected to kubernetes?\n")
		st.Dispatch(store.NewErrorAction(err))
		return
	}

	go w.dispatchJobChangesLoop(ctx, ch, st)
}

// When new UIDs are deployed, go through all our known Jobs and dispatch
// new events. This handles the case where we get the Job change event
// before the deploy id shows up in the manifest, which is way more common than
// you would think.
func (w *JobWatcher) setupNewUIDs(ctx context.Context, st store.RStore, newUIDs map[types.UID]model.ManifestName) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for uid, mn := range newUIDs {
		w.knownDeployedUIDs[uid] = mn

		Job, ok := w.knownJobs[uid]
		if !ok {
			continue
		}

		err := DispatchJobChange(st, Job, mn, w.nodeIP)
		if err != nil {
			logger.Get(ctx).Infof("error resolving Job url %s: %v", Job.Name, err)
		}
	}
}

// Match up the Job update to a manifest.
//
// The division between triageJobUpdate and recordJobUpdate is a bit artificial,
// but is designed this way to be consistent with PodWatcher and EventWatchManager.
func (w *JobWatcher) triageJobUpdate(Job *batchv1.Job) model.ManifestName {
	w.mu.Lock()
	defer w.mu.Unlock()

	uid := Job.UID
	w.knownJobs[uid] = Job

	manifestName, ok := w.knownDeployedUIDs[uid]
	if !ok {
		return ""
	}

	return manifestName
}

func (w *JobWatcher) dispatchJobChangesLoop(ctx context.Context, ch <-chan *batchv1.Job, st store.RStore) {
	for {
		select {
		case Job, ok := <-ch:
			if !ok {
				return
			}

			manifestName := w.triageJobUpdate(Job)
			if manifestName == "" {
				continue
			}

			err := DispatchJobChange(st, Job, manifestName, w.nodeIP)
			if err != nil {
				logger.Get(ctx).Infof("error resolving Job url %s: %v", Job.Name, err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func DispatchJobChange(st store.RStore, Job *batchv1.Job, mn model.ManifestName, ip k8s.NodeIP) error {
	// url, err := k8s.JobURL(Job, ip)
	// if err != nil {
	// 	return err
	// }

	// st.Dispatch(NewJobChangeAction(Job, mn, url))
	return nil
}
