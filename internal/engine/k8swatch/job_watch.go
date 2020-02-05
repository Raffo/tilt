package k8swatch

import (
	"context"
	"sync"

	"github.com/windmilleng/tilt/internal/k8s"
	"github.com/windmilleng/tilt/internal/store"
	"github.com/windmilleng/tilt/pkg/model"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

type JobWatcher struct {
	kCli         k8s.Client
	ownerFetcher k8s.OwnerFetcher

	mu                sync.RWMutex
	watches           []JobWatch
	knownDeployedUIDs map[types.UID]model.ManifestName

	// An index that maps the UID of Kubernetes resources to the UIDs of
	// all pods that they own (transitively).
	//
	// For example, a Deployment UID might contain a set of N pod UIDs.
	knownDescendentPodUIDs map[types.UID]store.UIDSet

	// An index of all the known pods, by UID
	knownJobs map[types.UID]*v1.Pod
}

func NewJobWatcher(kCli k8s.Client, ownerFetcher k8s.OwnerFetcher) *JobWatcher {
	return &JobWatcher{
		kCli:                   kCli,
		ownerFetcher:           ownerFetcher,
		knownDeployedUIDs:      make(map[types.UID]model.ManifestName),
		knownDescendentPodUIDs: make(map[types.UID]store.UIDSet),
	}
}

type JobWatch struct {
	name   model.ManifestName
	labels labels.Selector
	cancel context.CancelFunc
}
