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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/admission"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	node "k8s.io/kubernetes/pkg/apis/node"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

// newTestPlugin initializes the plugin in the order that the admission
// initializer uses. Cached Pods are served by the lister and the client serves
// live reads.
func newTestPlugin(t *testing.T, client kubernetes.Interface, cached ...*v1.Pod) *Plugin {
	t.Helper()
	p := &Plugin{Handler: admission.NewHandler(admission.Create)}
	p.InspectFeatureGates(utilfeature.DefaultFeatureGate)
	p.SetExternalKubeClientSet(client)
	factory := informers.NewSharedInformerFactory(client, 0)
	p.SetExternalKubeInformerFactory(factory)
	for _, pod := range cached {
		require.NoError(t, factory.Core().V1().Pods().Informer().GetIndexer().Add(pod))
	}
	require.NoError(t, p.ValidateInitialization())
	return p
}

func TestSourcePodCache(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLevelCheckpointRestore, true)
	running := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "ns", UID: "source-uid"}, Spec: v1.PodSpec{NodeName: "node"}, Status: v1.PodStatus{Phase: v1.PodRunning}}
	pending := running.DeepCopy()
	pending.Spec.NodeName = ""
	pending.Status.Phase = v1.PodPending
	previous := running.DeepCopy()
	previous.UID = "previous-uid"
	deleting := running.DeepCopy()
	deleting.DeletionTimestamp = new(metav1.Now())
	for _, tc := range []struct {
		name      string
		cached    *v1.Pod
		live      *v1.Pod
		uid       types.UID
		wantErr   string
		wantReads int
	}{
		{name: "cache hit", cached: running},
		{name: "cache miss", live: running, wantReads: 1},
		{name: "cached Pod not yet running", cached: pending, live: running, wantReads: 1},
		{name: "cached Pod replaced since", cached: previous, live: running, uid: running.UID, wantReads: 1},
		{name: "live read decides rejection", cached: pending, live: deleting, wantErr: "being deleted", wantReads: 1},
		{name: "missing", wantErr: "cannot read source", wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objects []runtime.Object
			if tc.live != nil {
				objects = append(objects, tc.live)
			}
			client := fake.NewClientset(objects...)
			var cached []*v1.Pod
			if tc.cached != nil {
				cached = append(cached, tc.cached)
			}
			p := newTestPlugin(t, client, cached...)
			checkpoint := &node.PodCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: "ns"}, Spec: node.PodCheckpointSpec{SourcePod: node.PodReference{Name: "source"}}}
			if tc.uid != "" {
				checkpoint.Spec.SourcePod.UID = &tc.uid
			}
			a := admission.NewAttributesRecord(checkpoint, nil, node.SchemeGroupVersion.WithKind("PodCheckpoint"), "ns", "checkpoint", node.SchemeGroupVersion.WithResource("podcheckpoints"), "", admission.Create, nil, false, nil)
			err := p.Validate(context.Background(), a, nil)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			require.Len(t, client.Actions(), tc.wantReads, "live reads: %v", client.Actions())
			for _, action := range client.Actions() {
				require.True(t, action.Matches("get", "pods"), "unexpected API request: %v", action)
			}
		})
	}
}

func TestValidateSourcePod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      func(*v1.Pod, *node.PodCheckpoint)
		missing     bool
		disabled    bool
		operation   admission.Operation
		subresource string
		wantErr     string
	}{
		{name: "running"},
		{name: "matching-uid", mutate: func(p *v1.Pod, c *node.PodCheckpoint) { c.Spec.SourcePod.UID = &p.UID }},
		{name: "mirror", mutate: func(p *v1.Pod, _ *node.PodCheckpoint) {
			p.Annotations = map[string]string{v1.MirrorPodAnnotationKey: "hash"}
		}, wantErr: "checkpointing static Pods is not supported"},
		{name: "mirror-matching-uid", mutate: func(p *v1.Pod, c *node.PodCheckpoint) {
			p.Annotations = map[string]string{v1.MirrorPodAnnotationKey: "hash"}
			c.Spec.SourcePod.UID = &p.UID
		}, wantErr: "checkpointing static Pods is not supported"},
		{name: "mirror-empty-annotation", mutate: func(p *v1.Pod, _ *node.PodCheckpoint) {
			p.Annotations = map[string]string{v1.MirrorPodAnnotationKey: ""}
		}, wantErr: "checkpointing static Pods is not supported"},
		{name: "disabled-mirror", disabled: true, mutate: func(p *v1.Pod, _ *node.PodCheckpoint) {
			p.Annotations = map[string]string{v1.MirrorPodAnnotationKey: "hash"}
		}},
		{name: "wrong-uid", mutate: func(_ *v1.Pod, c *node.PodCheckpoint) { c.Spec.SourcePod.UID = ptr.To(types.UID("other")) }, wantErr: "UID"},
		{name: "unassigned", mutate: func(p *v1.Pod, _ *node.PodCheckpoint) { p.Spec.NodeName = "" }, wantErr: "assigned to a node and Running"},
		{name: "pending", mutate: func(p *v1.Pod, _ *node.PodCheckpoint) { p.Status.Phase = v1.PodPending }, wantErr: "Running"},
		{name: "succeeded", mutate: func(p *v1.Pod, _ *node.PodCheckpoint) { p.Status.Phase = v1.PodSucceeded }, wantErr: "Running"},
		{name: "failed", mutate: func(p *v1.Pod, _ *node.PodCheckpoint) { p.Status.Phase = v1.PodFailed }, wantErr: "Running"},
		{name: "deleting", mutate: func(p *v1.Pod, _ *node.PodCheckpoint) { p.DeletionTimestamp = new(metav1.Now()) }, wantErr: "being deleted"},
		{name: "missing", missing: true, wantErr: "cannot read source"},
		{name: "missing-reference", mutate: func(_ *v1.Pod, c *node.PodCheckpoint) { c.Spec.SourcePod = node.PodReference{} }, wantErr: "sourcePod.name"},
		{name: "disabled", missing: true, disabled: true},
		{name: "update-does-not-revalidate-source", missing: true, operation: admission.Update},
		{name: "status-does-not-revalidate-source", missing: true, operation: admission.Update, subresource: "status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLevelCheckpointRestore, !tc.disabled)
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "ns", UID: "source-uid"}, Spec: v1.PodSpec{NodeName: "node"}, Status: v1.PodStatus{Phase: v1.PodRunning}}
			checkpoint := &node.PodCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: "ns"}, Spec: node.PodCheckpointSpec{SourcePod: node.PodReference{Name: "source"}}}
			if tc.mutate != nil {
				tc.mutate(pod, checkpoint)
			}
			var objects []runtime.Object
			if !tc.missing {
				objects = append(objects, pod)
			}
			client := fake.NewClientset(objects...)
			p := newTestPlugin(t, client)
			op := tc.operation
			if op == "" {
				op = admission.Create
			}
			a := admission.NewAttributesRecord(checkpoint, nil, node.SchemeGroupVersion.WithKind("PodCheckpoint"), "ns", "checkpoint", node.SchemeGroupVersion.WithResource("podcheckpoints"), tc.subresource, op, nil, false, nil)
			err := p.Validate(context.Background(), a, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if tc.disabled || op != admission.Create || tc.subresource != "" {
				if len(client.Actions()) != 0 {
					t.Fatalf("unexpected API requests: %v", client.Actions())
				}
			}
		})
	}
}
