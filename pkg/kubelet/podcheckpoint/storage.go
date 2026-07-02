/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package podcheckpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	nodev1alpha1 "k8s.io/api/node/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// podCheckpointDataSweepPeriod bounds how long data of a deleted PodCheckpoint
// stays after the capture or publication that used it has finished.
const podCheckpointDataSweepPeriod = time.Minute

type podCheckpointStorageInfo struct {
	CapacityBytes, AvailableBytes   uint64
	CapacityInodes, AvailableInodes uint64
	Isolated                        bool
}

// admitPodCheckpointStorage runs before probe suspension or runtime work. The
// filesystem capacity is the hard limit; headroom only avoids starting a capture
// that is already likely to fail. Retained archives remain charged by the kernel.
func (m *Manager) admitPodCheckpointStorage() error {
	policy := m.kubeletConfiguration.PodCheckpointStorage
	if policy == nil {
		return fmt.Errorf("configure podCheckpointStorage and mount a dedicated filesystem at %q before checkpointing", m.checkpointsDir)
	}
	inspect := m.podCheckpointStorageInfo
	if inspect == nil {
		inspect = inspectPodCheckpointStorage
	}
	info, err := inspect(m.checkpointsDir)
	if err != nil {
		return fmt.Errorf("inspect Pod checkpoint storage: %w", err)
	}
	if !info.Isolated {
		return fmt.Errorf("pod checkpoint storage %q must be a dedicated filesystem separate from the kubelet root", m.checkpointsDir)
	}
	if info.CapacityBytes == 0 || info.CapacityInodes == 0 || policy.MaxBytes <= 0 || policy.MaxInodes <= 0 || info.CapacityBytes > uint64(policy.MaxBytes) || info.CapacityInodes > uint64(policy.MaxInodes) {
		return fmt.Errorf("pod checkpoint filesystem capacity exceeds podCheckpointStorage or cannot be bounded")
	}
	if policy.MinAvailableBytes <= 0 || policy.MinAvailableInodes <= 0 || info.AvailableBytes < uint64(policy.MinAvailableBytes) || info.AvailableInodes < uint64(policy.MinAvailableInodes) {
		return fmt.Errorf("pod checkpoint storage has insufficient available bytes or inodes")
	}
	return nil
}

// sweepPodCheckpointData removes the data of PodCheckpoints that left this
// node's synced cache. Deleting the object releases its data; data of an
// existing object is never removed. Data stays while a capture or a
// publication may still use it.
func (m *Manager) sweepPodCheckpointData(ctx context.Context) error {
	state := m.podCheckpointWatch.Load()
	if state == nil || state.hasSynced == nil || !state.hasSynced() {
		return nil
	}
	checkpointsDir, err := filepath.Abs(m.checkpointsDir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(checkpointsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	deleted := make(map[types.UID]string)
	for _, entry := range entries {
		if uid, found := strings.CutPrefix(entry.Name(), podCheckpointDataPrefix); found && uid != "" {
			deleted[types.UID(uid)] = filepath.Join(checkpointsDir, entry.Name())
		}
	}
	// Read the cache after the directory: a capture creates data only for a
	// PodCheckpoint that the cache already holds.
	for _, object := range state.indexer.List() {
		if checkpoint, ok := object.(*nodev1alpha1.PodCheckpoint); ok {
			delete(deleted, checkpoint.UID)
		}
	}
	if len(deleted) == 0 {
		return nil
	}
	logger := klog.FromContext(ctx)
	for uid := range m.podCheckpointDataInUse() {
		delete(deleted, uid)
	}
	if len(deleted) == 0 || m.kubeClient == nil {
		return nil
	}
	// Removal cannot be undone, and the cache can lag behind the API server.
	listCtx, cancel := context.WithTimeout(ctx, podCheckpointAPITimeout)
	defer cancel()
	live, err := m.kubeClient.NodeV1alpha1().PodCheckpoints(metav1.NamespaceAll).List(listCtx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("status.nodeName", string(m.nodeName)).String(),
	})
	if err != nil {
		return fmt.Errorf("confirm that PodCheckpoints were deleted: %w", err)
	}
	for i := range live.Items {
		delete(deleted, live.Items[i].UID)
	}
	var errs []error
	for uid, path := range deleted {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
			continue
		}
		logger.V(2).Info("Removed data of a deleted PodCheckpoint", "podCheckpointUID", uid)
	}
	return errors.Join(errs...)
}

// podCheckpointDataInUse returns the UIDs of PodCheckpoints whose data a
// capture or a publication may still use.
func (m *Manager) podCheckpointDataInUse() sets.Set[types.UID] {
	inUse := sets.New[types.UID]()
	m.checkpointsInFlight.Range(func(_, checkpointUID any) bool {
		inUse.Insert(checkpointUID.(types.UID))
		return true
	})
	m.checkpointPublications.Range(func(checkpointUID, _ any) bool {
		inUse.Insert(checkpointUID.(types.UID))
		return true
	})
	return inUse
}
