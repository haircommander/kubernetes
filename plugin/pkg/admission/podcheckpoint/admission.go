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

// Package podcheckpoint validates source Pods before checkpoint creation.
package podcheckpoint

import (
	"context"
	"fmt"
	"io"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	initializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/component-base/featuregate"
	node "k8s.io/kubernetes/pkg/apis/node"
	"k8s.io/kubernetes/pkg/features"
)

// PluginName is the name of the checkpoint admission plugin.
const PluginName = "PodCheckpoint"

// Register registers the plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(io.Reader) (admission.Interface, error) {
		return &Plugin{Handler: admission.NewHandler(admission.Create)}, nil
	})
}

// Plugin requires a running source Pod before accepting a checkpoint request.
// The source Pod's state is a precondition at creation time rather than an
// invariant of the object, and the source kubelet checks the Pod again before
// capture, so this does not need to be enforced in REST storage.
type Plugin struct {
	*admission.Handler
	client                kubernetes.Interface
	podLister             corev1listers.PodLister
	enabled               bool
	inspectedFeatureGates bool
}

var (
	_ admission.ValidationInterface                = &Plugin{}
	_ initializer.WantsExternalKubeClientSet       = &Plugin{}
	_ initializer.WantsExternalKubeInformerFactory = &Plugin{}
	_ initializer.WantsFeatures                    = &Plugin{}
)

// SetExternalKubeClientSet supplies the client used to read source Pods.
func (p *Plugin) SetExternalKubeClientSet(client kubernetes.Interface) { p.client = client }

// SetExternalKubeInformerFactory supplies the Pod cache. Feature gates are
// inspected first, so a disabled plugin does not add a Pod informer.
func (p *Plugin) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	if p.enabled {
		p.podLister = f.Core().V1().Pods().Lister()
	}
}

// InspectFeatureGates configures the plugin for the checkpoint feature.
func (p *Plugin) InspectFeatureGates(gates featuregate.FeatureGate) {
	p.enabled = gates.Enabled(features.PodLevelCheckpointRestore)
	p.inspectedFeatureGates = true
}

// ValidateInitialization checks that admission dependencies were supplied.
func (p *Plugin) ValidateInitialization() error {
	if p.client == nil {
		return fmt.Errorf("%s requires a Kubernetes client", PluginName)
	}
	if !p.inspectedFeatureGates {
		return fmt.Errorf("%s requires feature gates", PluginName)
	}
	if p.enabled && p.podLister == nil {
		return fmt.Errorf("%s requires a Pod lister", PluginName)
	}
	return nil
}

// Validate rejects requests that cannot yet be processed by a source kubelet.
func (p *Plugin) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	if !p.enabled {
		return nil
	}
	if a.GetOperation() != admission.Create || a.GetSubresource() != "" || a.GetResource().GroupResource() != node.Resource("podcheckpoints") {
		return nil
	}
	checkpoint, ok := a.GetObject().(*node.PodCheckpoint)
	if !ok {
		return admission.NewForbidden(a, fmt.Errorf("expected PodCheckpoint, got %T", a.GetObject()))
	}
	if checkpoint.Spec.SourcePod.Name == "" {
		return admission.NewForbidden(a, fmt.Errorf("spec.sourcePod.name is required"))
	}
	ref := &checkpoint.Spec.SourcePod
	pod, err := p.getSourcePod(ctx, a.GetNamespace(), ref)
	if err != nil {
		return admission.NewForbidden(a, fmt.Errorf("cannot read source Pod %q: %w", ref.Name, err))
	}
	if err := validateSourcePod(pod, ref); err != nil {
		return admission.NewForbidden(a, err)
	}
	return nil
}

// getSourcePod returns the cached source Pod when it can be checkpointed. The
// cache can lag behind a Pod that was just created, started or replaced, or
// may not have synced yet, so a live read decides every other outcome.
func (p *Plugin) getSourcePod(ctx context.Context, namespace string, ref *node.PodReference) (*v1.Pod, error) {
	pod, err := p.podLister.Pods(namespace).Get(ref.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && validateSourcePod(pod, ref) == nil {
		return pod, nil
	}
	return p.client.CoreV1().Pods(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
}

// validateSourcePod checks that the source kubelet can checkpoint pod.
func validateSourcePod(pod *v1.Pod, ref *node.PodReference) error {
	if _, mirror := pod.Annotations[v1.MirrorPodAnnotationKey]; mirror {
		return fmt.Errorf("source Pod %q is a mirror Pod; checkpointing static Pods is not supported", ref.Name)
	}
	if ref.UID != nil && *ref.UID != pod.UID {
		return fmt.Errorf("source Pod %q has UID %q, not requested UID %q", ref.Name, pod.UID, *ref.UID)
	}
	if pod.DeletionTimestamp != nil {
		return fmt.Errorf("source Pod %q is being deleted", ref.Name)
	}
	if pod.Spec.NodeName == "" || pod.Status.Phase != v1.PodRunning {
		return fmt.Errorf("source Pod %q must be assigned to a node and Running before checkpointing", ref.Name)
	}
	return nil
}
