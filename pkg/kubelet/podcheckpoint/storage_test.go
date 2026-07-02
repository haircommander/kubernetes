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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	nodev1alpha1 "k8s.io/api/node/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/ktesting"
)

func TestPodCheckpointStorageAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*Manager, *podCheckpointStorageInfo)
		want   string
	}{
		{name: "bounded storage"},
		{name: "missing policy", modify: func(kl *Manager, _ *podCheckpointStorageInfo) { kl.kubeletConfiguration.PodCheckpointStorage = nil }, want: "configure podCheckpointStorage"},
		{name: "shared filesystem", modify: func(_ *Manager, info *podCheckpointStorageInfo) { info.Isolated = false }, want: "dedicated filesystem"},
		{name: "unbounded inodes", modify: func(_ *Manager, info *podCheckpointStorageInfo) { info.CapacityInodes = 0 }, want: "cannot be bounded"},
		{name: "byte capacity", modify: func(_ *Manager, info *podCheckpointStorageInfo) { info.CapacityBytes = 2 << 30 }, want: "capacity exceeds"},
		{name: "inode capacity", modify: func(_ *Manager, info *podCheckpointStorageInfo) { info.CapacityInodes = 10001 }, want: "capacity exceeds"},
		{name: "byte pressure", modify: func(_ *Manager, info *podCheckpointStorageInfo) { info.AvailableBytes = 0 }, want: "insufficient"},
		{name: "inode pressure", modify: func(_ *Manager, info *podCheckpointStorageInfo) { info.AvailableInodes = 0 }, want: "insufficient"},
		{name: "inspection error", modify: func(kl *Manager, _ *podCheckpointStorageInfo) {
			kl.podCheckpointStorageInfo = func(string) (podCheckpointStorageInfo, error) {
				return podCheckpointStorageInfo{}, errors.New("statfs failed")
			}
		}, want: "statfs failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestManager(t)
			defer fixture.Cleanup()
			kl := fixture.manager
			useCheckpointStorage(kl)
			info, err := kl.podCheckpointStorageInfo("")
			require.NoError(t, err)
			kl.podCheckpointStorageInfo = func(string) (podCheckpointStorageInfo, error) { return info, nil }
			if test.modify != nil {
				test.modify(kl, &info)
			}
			err = kl.admitPodCheckpointStorage()
			if test.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.want)
			}
		})
	}
}

func TestPodCheckpointDataSweep(t *testing.T) {
	ctx := ktesting.Init(t)
	fixture := newTestManager(t)
	defer fixture.Cleanup()
	kl := fixture.manager
	live := &nodev1alpha1.PodCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "live", UID: "live"}}
	client := fake.NewClientset(live)
	kl.kubeClient = client
	seedCheckpointCache(t, kl)
	// The cache has not observed this PodCheckpoint yet.
	lagging := &nodev1alpha1.PodCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "lagging", UID: "lagging"}}
	require.NoError(t, client.Tracker().Add(lagging))
	dataPath := func(uid types.UID) string {
		_, path, err := kl.podCheckpointOutputPath(uid)
		require.NoError(t, err)
		return path
	}
	for _, uid := range []types.UID{"live", "lagging", "deleted", "capturing", "publishing"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dataPath(uid), "images"), 0o700))
	}
	// The checkpoint filesystem can also hold fsck output.
	others := []string{filepath.Join(kl.checkpointsDir, "lost+found")}
	for _, dir := range others {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	kl.checkpointsInFlight.Store(types.UID("capturing-source-uid"), types.UID("capturing"))
	kl.checkpointPublications.Store(types.UID("publishing"), struct{}{})

	require.NoError(t, kl.sweepPodCheckpointData(ctx))
	require.NoDirExists(t, dataPath("deleted"))
	for _, uid := range []types.UID{"live", "lagging", "capturing", "publishing"} {
		require.DirExists(t, dataPath(uid), "data of an existing or busy PodCheckpoint must stay")
	}
	for _, dir := range others {
		require.DirExists(t, dir)
	}

	// Data released after its PodCheckpoint was deleted goes on a later pass.
	kl.checkpointsInFlight.Delete(types.UID("capturing-source-uid"))
	kl.checkpointPublications.Delete(types.UID("publishing"))
	require.NoError(t, kl.sweepPodCheckpointData(ctx))
	for _, uid := range []types.UID{"capturing", "publishing"} {
		require.NoDirExists(t, dataPath(uid))
	}
	require.DirExists(t, dataPath("live"))
	require.DirExists(t, dataPath("lagging"))
}

func TestPodCheckpointDataSweepKeepsDataOfUnknownUse(t *testing.T) {
	for _, reason := range []string{"cache not synced", "API unavailable"} {
		t.Run(reason, func(t *testing.T) {
			ctx := ktesting.Init(t)
			fixture := newTestManager(t)
			defer fixture.Cleanup()
			kl := fixture.manager
			client := fake.NewClientset()
			kl.kubeClient = client
			seedCheckpointCache(t, kl)
			wantErr := false
			switch reason {
			case "cache not synced":
				kl.podCheckpointWatch.Load().hasSynced = func() bool { return false }
			case "API unavailable":
				client.PrependReactor("list", "podcheckpoints", func(clienttesting.Action) (bool, apiruntime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("restarting")
				})
				wantErr = true
			}
			_, path, err := kl.podCheckpointOutputPath("deleted")
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(path, 0o700))

			err = kl.sweepPodCheckpointData(ctx)
			if wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.DirExists(t, path)
		})
	}
}

func TestPodCheckpointStorageRejectionPrecedesProbeSuspension(t *testing.T) {
	for _, reason := range []string{"storage headroom", "missing policy"} {
		t.Run(reason, func(t *testing.T) {
			kl, runtime, pod, pc := checkpointFixture(t)
			var runtimeCalls, probeCalls atomic.Int32
			kl.containerRuntime = &checkpointRuntime{Runtime: runtime, checkpointPod: func(context.Context, *runtimeapi.CheckpointPodRequest) error { runtimeCalls.Add(1); return nil }}
			kl.probeManager = &checkpointProbeManager{suspend: func(context.Context, types.UID) error { probeCalls.Add(1); return nil }, resumed: make(chan types.UID, 1)}
			switch reason {
			case "storage headroom":
				kl.podCheckpointStorageInfo = func(string) (podCheckpointStorageInfo, error) {
					return podCheckpointStorageInfo{Isolated: true, CapacityBytes: 1 << 30, CapacityInodes: 10000}, nil
				}
			case "missing policy":
				kl.kubeletConfiguration.PodCheckpointStorage = nil
			}
			require.NoError(t, checkpointPod(t.Context(), kl, pod, pc.Name, pc.UID, time.Minute))
			require.Eventually(t, func() bool {
				current, err := kl.kubeClient.NodeV1alpha1().PodCheckpoints(pc.Namespace).Get(t.Context(), pc.Name, metav1.GetOptions{})
				return err == nil && len(current.Status.Conditions) > 0
			}, 5*time.Second, 10*time.Millisecond)
			require.Zero(t, runtimeCalls.Load())
			require.Zero(t, probeCalls.Load())
			_, path, err := kl.podCheckpointOutputPath(pc.UID)
			require.NoError(t, err)
			require.NoDirExists(t, path)
		})
	}
}
