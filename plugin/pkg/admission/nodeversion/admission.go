/*
Copyright 2017 The Kubernetes Authors.

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

package noderestriction

import (
	"context"
	"fmt"
	"io"

	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission"
	apiserveradmission "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/component-base/featuregate"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/auth/nodeidentifier"
)

// PluginName is a string with the name of the plugin
const PluginName = "NodeVersion"

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return NewPlugin(nodeidentifier.NewDefaultNodeIdentifier(), config)
	})
}

// NewPlugin creates a new NodeRestriction admission plugin.
// This plugin identifies requests from nodes
// NewPlugin creates a new NodeVersion admission plugin from the provided config file.
// The config file is specified by --admission-control-config-file and has the
// following format for a webhook:
//
//	{
//	  "nodeVersionPolicy": {
//	     "minimumKubeletVerison": "4.18.0",
//	  }
//	}
//
// The config file may be json or yaml.
func NewPlugin(nodeIdentifier nodeidentifier.NodeIdentifier, configFile io.Reader) (*Plugin, error) {
	if configFile == nil {
		return nil, fmt.Errorf("no config specified")
	}

	// TODO: move this to a versioned configuration file format
	var config VersionConfig
	d := yaml.NewYAMLOrJSONDecoder(configFile, 4096)
	err := d.Decode(&config)
	if err != nil {
		return nil, err
	}

	minKubeletVersion, err := utilversion.ParseSemantic(config.NodeVersionPolicy.MinimumKubeletVersion)
	if err != nil {
		return nil, err
	}

	return &Plugin{
		Handler:           admission.NewHandler(admission.Create, admission.Update, admission.Delete),
		nodeIdentifier:    nodeIdentifier,
		minKubeletVersion: minKubeletVersion,
	}, nil
}

// VersionConfig holds config data for admission controllers
type VersionConfig struct {
	NodeVersionPolicy nodeVersionPolicy `json:"nodeVersionPolicy"`
}

// nodeVersionPolicy holds config data for the node version policy
type nodeVersionPolicy struct {
	MinimumKubeletVersion string `json:"minimumKubeletVersion"`
}

// Plugin holds state for and implements the admission plugin.
type Plugin struct {
	*admission.Handler
	nodeIdentifier    nodeidentifier.NodeIdentifier
	nodesGetter       corev1lister.NodeLister
	minKubeletVersion *utilversion.Version
}

var (
	_ admission.Interface                                 = &Plugin{}
	_ apiserveradmission.WantsExternalKubeInformerFactory = &Plugin{}
	_ apiserveradmission.WantsFeatures                    = &Plugin{}
)

// InspectFeatureGates allows setting bools without taking a dep on a global variable
func (p *Plugin) InspectFeatureGates(featureGates featuregate.FeatureGate) {}

// SetExternalKubeInformerFactory registers an informer factory into Plugin
func (p *Plugin) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	p.nodesGetter = f.Core().V1().Nodes().Lister()
}

// ValidateInitialization validates the Plugin was initialized properly
func (p *Plugin) ValidateInitialization() error {
	if p.nodeIdentifier == nil {
		return fmt.Errorf("%s requires a node identifier", PluginName)
	}
	if p.nodesGetter == nil {
		return fmt.Errorf("%s requires a node getter", PluginName)
	}
	return nil
}

var (
	nodeResource = api.Resource("nodes")
)

// Admit checks the admission policy and triggers corresponding actions
func (p *Plugin) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	nodeName, isNode := p.nodeIdentifier.NodeIdentity(a.GetUserInfo())

	// Our job is just to restrict nodes
	if !isNode {
		return nil
	}

	if len(nodeName) == 0 {
		// disallow requests we cannot match to a particular node
		return admission.NewForbidden(a, fmt.Errorf("could not determine node from user %q", a.GetUserInfo().GetName()))
	}

	// TODO: if node doesn't exist and this isn't a create node request, then reject.

	switch a.GetResource().GroupResource() {
	case nodeResource:
		return p.admitNode(nodeName, a)

	default:
		return nil
	}
}

func (p *Plugin) admitNode(nodeName string, a admission.Attributes) error {
	requestedName := a.GetName()

	if requestedName != nodeName {
		return admission.NewForbidden(a, fmt.Errorf("node %q is not allowed to modify node %q", nodeName, requestedName))
	}

	if a.GetOperation() == admission.Create {
		node, ok := a.GetObject().(*api.Node)
		if !ok {
			return admission.NewForbidden(a, fmt.Errorf("unexpected type %T", a.GetObject()))
		}
		if err := p.validateVersion(node); err != nil {
			return admission.NewForbidden(a, err)
		}
	}

	if a.GetOperation() == admission.Update {
		node, ok := a.GetObject().(*api.Node)
		if !ok {
			return admission.NewForbidden(a, fmt.Errorf("unexpected type %T", a.GetObject()))
		}
		// TODO FIXME: do we need the old?
		// oldNode, ok := a.GetOldObject().(*api.Node)
		// if !ok {
		// 	return admission.NewForbidden(a, fmt.Errorf("unexpected type %T", a.GetObject()))
		// }
		if err := p.validateVersion(node); err != nil {
			return admission.NewForbidden(a, err)
		}
	}

	return nil
}

func (p *Plugin) validateVersion(node *api.Node) error {
	givenVersion, err := utilversion.ParseSemantic(node.Status.NodeInfo.KubeletVersion)
	if err != nil {
		return fmt.Errorf("unexpected version %s: %w", node.Status.NodeInfo.KubeletVersion, err)
	}

	if !givenVersion.AtLeast(p.minKubeletVersion) {
		return fmt.Errorf("registered kubelet version %s lower than configured minimum %s", givenVersion.String(), p.minKubeletVersion.String())
	}
	return nil
}
