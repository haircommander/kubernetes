// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1

import (
	v1 "github.com/openshift/api/network/v1"
)

// EgressNetworkPolicyRuleApplyConfiguration represents an declarative configuration of the EgressNetworkPolicyRule type for use
// with apply.
type EgressNetworkPolicyRuleApplyConfiguration struct {
	Type *v1.EgressNetworkPolicyRuleType            `json:"type,omitempty"`
	To   *EgressNetworkPolicyPeerApplyConfiguration `json:"to,omitempty"`
}

// EgressNetworkPolicyRuleApplyConfiguration constructs an declarative configuration of the EgressNetworkPolicyRule type for use with
// apply.
func EgressNetworkPolicyRule() *EgressNetworkPolicyRuleApplyConfiguration {
	return &EgressNetworkPolicyRuleApplyConfiguration{}
}

// WithType sets the Type field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Type field is set to the value of the last call.
func (b *EgressNetworkPolicyRuleApplyConfiguration) WithType(value v1.EgressNetworkPolicyRuleType) *EgressNetworkPolicyRuleApplyConfiguration {
	b.Type = &value
	return b
}

// WithTo sets the To field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the To field is set to the value of the last call.
func (b *EgressNetworkPolicyRuleApplyConfiguration) WithTo(value *EgressNetworkPolicyPeerApplyConfiguration) *EgressNetworkPolicyRuleApplyConfiguration {
	b.To = value
	return b
}