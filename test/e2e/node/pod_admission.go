/*
Copyright 2024 The Kubernetes Authors.

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

package node

import (
	"context"
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("PodRejectionStatus", func() {
	f := framework.NewDefaultFramework("pod-rejection-status")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	ginkgo.Context("Kubelet", func() {
		ginkgo.It("should reject pod when the node didn't have enough resource", func(ctx context.Context) {
			node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
			framework.ExpectNoError(err, "Failed to get a ready schedulable node")

			// Create a pod that requests more CPU than the node has
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-out-of-cpu",
					Namespace: f.Namespace.Name,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "pod-out-of-cpu",
							Image: imageutils.GetPauseImageName(),
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceCPU: resource.MustParse("1000000000000"), // requests more CPU than any node has
								},
							},
						},
					},
				},
			}

			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// Wait for the scheduler to update the pod status
			err = e2epod.WaitForPodNameUnschedulableInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace)
			framework.ExpectNoError(err)

			// Fetch the pod to get the latest status which should be last one observed by the scheduler
			// before it rejected the pod
			pod, err = f.ClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			// force assign the Pod to a node in order to get rejection status later
			binding := &v1.Binding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pod.Name,
					Namespace: pod.Namespace,
					UID:       pod.UID,
				},
				Target: v1.ObjectReference{
					Kind: "Node",
					Name: node.Name,
				},
			}
			err = f.ClientSet.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			//			done := make(chan struct{}, 1)
			//			go func() {

			errC := make(chan error, 1)
			go func() {
				// kubelet has rejected the pod
				err = e2epod.WaitForPodFailedReason(ctx, f.ClientSet, pod, "OutOfcpu", f.Timeouts.PodStartShort)
				if err != nil {
					errC <- err
					return
				}

				// fetch the reject Pod and compare the status
				_, err := f.ClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
				errC <- err
			}()
			count := 0
			gomega.Consistently(func() bool {
				pod2 := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-out-of-cpu" + strconv.Itoa(count),
						Namespace: f.Namespace.Name,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:  "pod-out-of-cpu",
								Image: imageutils.GetPauseImageName(),
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU: resource.MustParse("100m"), // requests more CPU than any node has
									},
								},
							},
						},
					},
				}
				count += 1
				pod2, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(ctx, pod2, metav1.CreateOptions{})
				framework.ExpectNoError(err)
				err = e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod2)
				framework.ExpectNoError(err)

				return true
			}, 2*time.Minute, 5*time.Millisecond).Should(gomega.BeTrue())
			if err := <-errC; err != nil {
				framework.ExpectNoError(err)
			}
		})
	})
})
