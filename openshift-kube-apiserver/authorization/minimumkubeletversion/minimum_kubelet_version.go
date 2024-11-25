package minimumkubeletversion

import (
	"context"
	"fmt"

	"github.com/blang/semver/v4"
	nodelib "github.com/openshift/library-go/pkg/apiserver/node"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	v1listers "k8s.io/client-go/listers/core/v1"
	cache "k8s.io/client-go/tools/cache"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/auth/nodeidentifier"
)

type minimumKubeletVersionAuth struct {
	nodeIdentifier nodeidentifier.NodeIdentifier
	nodeInformer   cache.SharedIndexInformer
	nodeLister     v1listers.NodeLister
	minVersion     *semver.Version
}

// Creates a new minimumKubeletVersionAuth object, which is an authorizer that checks
// whether nodes are new enough to be authorized.
func NewMinimumKubeletVersion(minVersion *semver.Version,
	nodeIdentifier nodeidentifier.NodeIdentifier,
	nodeInformer cache.SharedIndexInformer,
	nodeLister v1listers.NodeLister,
) *minimumKubeletVersionAuth {
	return &minimumKubeletVersionAuth{
		nodeIdentifier: nodeIdentifier,
		nodeInformer:   nodeInformer,
		nodeLister:     nodeLister,
		minVersion:     minVersion,
	}
}

func (m *minimumKubeletVersionAuth) Authorize(ctx context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
	if m.minVersion == nil {
		return authorizer.DecisionNoOpinion, "", nil
	}

	nodeName, isNode := m.nodeIdentifier.NodeIdentity(attrs.GetUser())
	if !isNode {
		// ignore requests from non-nodes
		return authorizer.DecisionNoOpinion, "", nil
	}

	if len(nodeName) == 0 {
		return authorizer.DecisionNoOpinion, fmt.Sprintf("unknown node for user %q", attrs.GetUser().GetName()), nil
	}

	// Short-circut if "subjectaccessreviews", or a "get" or "update" on the node object.
	// Regardless of kubelet version, it should be allowed to do these things.
	if attrs.IsResourceRequest() {
		requestResource := schema.GroupResource{Group: attrs.GetAPIGroup(), Resource: attrs.GetResource()}
		switch requestResource {
		case api.Resource("nodes"):
			if v := attrs.GetVerb(); v == "get" || v == "update" {
				return authorizer.DecisionNoOpinion, "", nil
			}
		// TODO(haircommander): do we need other flavors of access reviews here?
		case api.Resource("subjectaccessreviews"):
			return authorizer.DecisionNoOpinion, "", nil
		}
	}

	node, err := m.nodeLister.Get(nodeName)
	if err != nil {
		return authorizer.DecisionNoOpinion, fmt.Sprintf("failed to get node %s: %v", nodeName, err), nil
	}

	if err := nodelib.IsNodeTooOld(node, m.minVersion); err != nil {
		return authorizer.DecisionDeny, err.Error(), nil
	}

	return authorizer.DecisionNoOpinion, "", nil
}
