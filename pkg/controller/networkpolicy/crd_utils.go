// Copyright 2020 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package networkpolicy

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/apis/controlplane"
	"antrea.io/antrea/pkg/apis/crd/v1alpha1"
	crdv1alpha3 "antrea.io/antrea/pkg/apis/crd/v1alpha3"
	"antrea.io/antrea/pkg/controller/networkpolicy/store"
	antreatypes "antrea.io/antrea/pkg/controller/types"
	"antrea.io/antrea/pkg/util/k8s"
)

var (
	// matchAllPodsPeerCrd is a v1alpha1.NetworkPolicyPeer matching all
	// Pods from all Namespaces.
	matchAllPodsPeerCrd = v1alpha1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{},
	}
)

// semanticIgnoreLastTransitionTime does semantic deep equality checks for
// NetworkPolicyCondition but excludes LastTransitionTime. They are used when
// comparing NetworkPolicyCondition in NetworkPolicyStatus objects to avoid
// unnecessary updates caused different status generation time.
var semanticIgnoreLastTransitionTime = conversion.EqualitiesOrDie(
	func(a, b v1alpha1.NetworkPolicyCondition) bool {
		a.LastTransitionTime = metav1.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
		b.LastTransitionTime = metav1.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
		return a == b
	},
)

// NetworkPolicyStatusEqual compares two NetworkPolicyStatus objects. It disregards
// the LastTransitionTime field in the status Conditions.
func NetworkPolicyStatusEqual(oldStatus, newStatus v1alpha1.NetworkPolicyStatus) bool {
	return semanticIgnoreLastTransitionTime.DeepEqual(oldStatus, newStatus)
}

// groupMembersComputedConditionEqual checks whether the condition status for GroupMembersComputed condition
// is same. Returns true if equal, otherwise returns false. It disregards the lastTransitionTime field.
func groupMembersComputedConditionEqual(conds []crdv1alpha3.GroupCondition, condition crdv1alpha3.GroupCondition) bool {
	for _, c := range conds {
		if c.Type == crdv1alpha3.GroupMembersComputed {
			if c.Status == condition.Status {
				return true
			}
		}
	}
	return false
}

// toAntreaServicesForCRD converts a slice of v1alpha1.NetworkPolicyPort objects
// and a slice of v1alpha1.NetworkPolicyProtocol objects to a slice of Antrea
// Service objects. A bool is returned along with the Service objects to indicate
// whether any named port exists.
func toAntreaServicesForCRD(npPorts []v1alpha1.NetworkPolicyPort, npProtocols []v1alpha1.NetworkPolicyProtocol) ([]controlplane.Service, bool) {
	var antreaServices []controlplane.Service
	var namedPortExists bool
	for _, npPort := range npPorts {
		if npPort.Port != nil && npPort.Port.Type == intstr.String {
			namedPortExists = true
		}
		antreaServices = append(antreaServices, controlplane.Service{
			Protocol: toAntreaProtocol(npPort.Protocol),
			Port:     npPort.Port,
			EndPort:  npPort.EndPort,
		})
	}
	for _, npProtocol := range npProtocols {
		if npProtocol.ICMP != nil {
			curProtocol := controlplane.ProtocolICMP
			antreaServices = append(antreaServices, controlplane.Service{
				Protocol: &curProtocol,
				ICMPType: npProtocol.ICMP.ICMPType,
				ICMPCode: npProtocol.ICMP.ICMPCode,
			})
		}
		if npProtocol.IGMP != nil {
			curProtocol := controlplane.ProtocolIGMP
			antreaServices = append(antreaServices, controlplane.Service{
				Protocol:     &curProtocol,
				IGMPType:     npProtocol.IGMP.IGMPType,
				GroupAddress: npProtocol.IGMP.GroupAddress,
			})
		}
	}
	return antreaServices, namedPortExists
}

// toAntreaIPBlockForCRD converts a v1alpha1.IPBlock to an Antrea IPBlock.
func toAntreaIPBlockForCRD(ipBlock *v1alpha1.IPBlock) (*controlplane.IPBlock, error) {
	// Convert the allowed IPBlock to networkpolicy.IPNet.
	ipNet, err := cidrStrToIPNet(ipBlock.CIDR)
	if err != nil {
		return nil, err
	}
	antreaIPBlock := &controlplane.IPBlock{
		CIDR: *ipNet,
		// secv1alpha.IPBlock does not have the Except slices.
		Except: []controlplane.IPNet{},
	}
	return antreaIPBlock, nil
}

// toAntreaPeerForCRD creates a Antrea controlplane NetworkPolicyPeer for crdv1alpha1 NetworkPolicyPeer.
// It is used when peer's Namespaces are not matched by NamespaceMatchTypes, for which the controlplane
// NetworkPolicyPeers will need to be created on a per Namespace basis.
func (n *NetworkPolicyController) toAntreaPeerForCRD(peers []v1alpha1.NetworkPolicyPeer,
	np metav1.Object, dir controlplane.Direction, namedPortExists bool) *controlplane.NetworkPolicyPeer {
	var addressGroups []string
	// NetworkPolicyPeer is supposed to match all addresses when it is empty and no clusterGroup is present.
	// It's treated as an IPBlock "0.0.0.0/0".
	if len(peers) == 0 {
		// For an egress Peer that specifies any named ports, it creates or
		// reuses the AddressGroup matching all Pods in all Namespaces and
		// appends the AddressGroup UID to the returned Peer such that it can be
		// used to resolve the named ports.
		// For other cases it uses the IPBlock "0.0.0.0/0" to avoid the overhead
		// of handling member updates of the AddressGroup.
		if dir == controlplane.DirectionIn || !namedPortExists {
			return &matchAllPeer
		}
		allPodsGroupUID := n.createAddressGroup("", matchAllPodsPeerCrd.PodSelector, matchAllPodsPeerCrd.NamespaceSelector, nil, nil)
		podsPeer := matchAllPeer
		podsPeer.AddressGroups = append(addressGroups, allPodsGroupUID)
		return &podsPeer
	}
	var ipBlocks []controlplane.IPBlock
	var fqdns []string
	for _, peer := range peers {
		// A v1alpha1.NetworkPolicyPeer will either have an IPBlock or FQDNs or a
		// podSelector and/or namespaceSelector set or a reference to the
		// ClusterGroup.
		if peer.IPBlock != nil {
			ipBlock, err := toAntreaIPBlockForCRD(peer.IPBlock)
			if err != nil {
				klog.Errorf("Failure processing Antrea NetworkPolicy %s/%s IPBlock %v: %v", np.GetNamespace(), np.GetName(), peer.IPBlock, err)
				continue
			}
			ipBlocks = append(ipBlocks, *ipBlock)
		} else if peer.Group != "" {
			normalizedUID, groupIPBlocks := n.processRefGroupOrClusterGroup(peer.Group, np.GetNamespace())
			if normalizedUID != "" {
				addressGroups = append(addressGroups, normalizedUID)
			}
			ipBlocks = append(ipBlocks, groupIPBlocks...)
		} else if peer.FQDN != "" {
			fqdns = append(fqdns, peer.FQDN)
		} else if peer.ServiceAccount != nil {
			normalizedUID := n.createAddressGroup(peer.ServiceAccount.Namespace, serviceAccountNameToPodSelector(peer.ServiceAccount.Name), nil, nil, nil)
			addressGroups = append(addressGroups, normalizedUID)
		} else if peer.NodeSelector != nil {
			normalizedUID := n.createAddressGroup("", nil, nil, nil, peer.NodeSelector)
			addressGroups = append(addressGroups, normalizedUID)
		} else {
			normalizedUID := n.createAddressGroup(np.GetNamespace(), peer.PodSelector, peer.NamespaceSelector, peer.ExternalEntitySelector, nil)
			addressGroups = append(addressGroups, normalizedUID)
		}
	}
	return &controlplane.NetworkPolicyPeer{AddressGroups: addressGroups, IPBlocks: ipBlocks, FQDNs: fqdns}
}

// toNamespacedPeerForCRD creates an Antrea controlplane NetworkPolicyPeer for crdv1alpha1 NetworkPolicyPeer
// for a particular Namespace. It is used when a single crdv1alpha1 NetworkPolicyPeer maps to multiple
// controlplane NetworkPolicyPeers because the appliedTo workloads reside in different Namespaces.
func (n *NetworkPolicyController) toNamespacedPeerForCRD(peers []v1alpha1.NetworkPolicyPeer, namespace string) *controlplane.NetworkPolicyPeer {
	var addressGroups []string
	for _, peer := range peers {
		normalizedUID := n.createAddressGroup(namespace, peer.PodSelector, nil, peer.ExternalEntitySelector, nil)
		addressGroups = append(addressGroups, normalizedUID)
	}
	return &controlplane.NetworkPolicyPeer{AddressGroups: addressGroups}
}

// svcRefToPeerForCRD creates an Antrea controlplane NetworkPolicyPeer from
// ServiceReference in ToServices field. For ANP, we will use the
// defaultNamespace(policy Namespace) as the Namespace of ServiceReference that
// doesn't set Namespace.
func (n *NetworkPolicyController) svcRefToPeerForCRD(svcRefs []v1alpha1.NamespacedName, defaultNamespace string) *controlplane.NetworkPolicyPeer {
	var controlplaneSvcRefs []controlplane.ServiceReference
	for _, svcRef := range svcRefs {
		curNS := defaultNamespace
		if svcRef.Namespace != "" {
			curNS = svcRef.Namespace
		}
		controlplaneSvcRefs = append(controlplaneSvcRefs, controlplane.ServiceReference{
			Namespace: curNS,
			Name:      svcRef.Name,
		})
	}
	return &controlplane.NetworkPolicyPeer{ToServices: controlplaneSvcRefs}
}

// createAppliedToGroupForInternalGroup creates an AppliedToGroup object corresponding to an
// internal Group. If the AppliedToGroup already exists, it returns the key
// otherwise it copies the internal Group contents to an AppliedToGroup resource and returns
// its key.
func (n *NetworkPolicyController) createAppliedToGroupForInternalGroup(intGrp *antreatypes.Group) string {
	key, err := store.GroupKeyFunc(intGrp)
	if err != nil {
		return ""
	}
	// Check to see if the AppliedToGroup already exists
	_, found, _ := n.appliedToGroupStore.Get(key)
	if found {
		return key
	}
	// Create an AppliedToGroup object for this internal Group.
	appliedToGroup := &antreatypes.AppliedToGroup{
		UID:  intGrp.UID,
		Name: key,
	}
	klog.V(2).InfoS("Creating new AppliedToGroup corresponding to internal Group", "AppliedToGroup", appliedToGroup.UID, "internalGroup", intGrp.SourceReference.ToTypedString())
	n.appliedToGroupStore.Create(appliedToGroup)
	n.enqueueAppliedToGroup(key)
	return key
}

// createAppliedToGroupForService creates an AppliedToGroup object corresponding to a Service if it is not created already.
func (n *NetworkPolicyController) createAppliedToGroupForService(service *v1alpha1.NamespacedName) string {
	key := getNormalizedUID(k8s.NamespacedName(service.Namespace, service.Name))
	// Check to see if the AppliedToGroup already exists
	_, found, _ := n.appliedToGroupStore.Get(key)
	if found {
		return key
	}
	// Create an AppliedToGroup object for this Service.
	appliedToGroup := &antreatypes.AppliedToGroup{
		UID:  types.UID(key),
		Name: key,
		Service: &controlplane.ServiceReference{
			Namespace: service.Namespace,
			Name:      service.Name,
		},
	}
	klog.V(2).Infof("Creating new AppliedToGroup %v corresponding to a Service %s", appliedToGroup.UID, k8s.NamespacedName(service.Namespace, service.Name))
	n.appliedToGroupStore.Create(appliedToGroup)
	n.enqueueAppliedToGroup(key)
	return key
}

// createAddressGroupForClusterGroupCRD creates an AddressGroup object corresponding to a
// ClusterGroup spec. If the AddressGroup already exists, it returns the key
// otherwise it copies the ClusterGroup CRD contents to an AddressGroup resource and returns
// its key. If the corresponding internal Group is not found return empty.
func (n *NetworkPolicyController) createAddressGroupForInternalGroup(intGrp *antreatypes.Group) string {
	key, err := store.GroupKeyFunc(intGrp)
	if err != nil {
		return ""
	}
	// Check to see if the AddressGroup already exists
	_, found, _ := n.addressGroupStore.Get(key)
	if found {
		return key
	}
	// Create an AddressGroup object for this Cluster Group.
	addressGroup := &antreatypes.AddressGroup{
		UID:  intGrp.UID,
		Name: key,
	}
	n.addressGroupStore.Create(addressGroup)
	klog.V(2).InfoS("Created new AddressGroup corresponding to internal Group", "AddressGroup", addressGroup.UID, "internalGroup", intGrp.SourceReference.ToTypedString())
	return key
}

// getTierPriority retrieves the priority associated with the input Tier name.
// If the Tier name is empty, by default, the lowest priority Application Tier
// is returned.
func (n *NetworkPolicyController) getTierPriority(tier string) int32 {
	if tier == "" {
		return DefaultTierPriority
	}
	// If the tier name is part of the static tier name set, we need to convert
	// tier name to lowercase to match the corresponding Tier CRD name. This is
	// possible in case of upgrade where in a previously created Antrea Policy
	// CRD was referring to an old static tier. Static tiers were introduced in
	// release 0.9.0 and deprecated in 0.10.0. So any upgrade from 0.9.0 to a
	// later release will undergo this conversion.
	if staticTierSet.Has(tier) {
		tier = strings.ToLower(tier)
	}
	t, err := n.tierLister.Get(tier)
	if err != nil {
		// This error should ideally not occur as we perform validation.
		klog.Errorf("Failed to retrieve Tier %s. Setting default tier priority: %v", tier, err)
		return DefaultTierPriority
	}
	return t.Spec.Priority
}

// getNormalizedNameForSelector retrieves the normalized name for GroupSelector.
// If the GroupSelector is nil, an empty string is returned.
func getNormalizedNameForSelector(sel *antreatypes.GroupSelector) string {
	if sel != nil {
		return sel.NormalizedName
	}
	return ""
}

func (n *NetworkPolicyController) syncInternalGroup(key string) error {
	defer n.triggerANPUpdates(key)
	defer n.triggerCNPUpdates(key)
	defer n.triggerParentGroupSync(key)
	// Retrieve the internal Group corresponding to this key.
	grpObj, found, _ := n.internalGroupStore.Get(key)
	if !found {
		klog.V(2).InfoS("Internal group not found.", "internalGroup", key)
		n.groupingInterface.DeleteGroup(internalGroupType, key)
		return nil
	}
	grp := grpObj.(*antreatypes.Group)
	if grp.SourceReference.Namespace != "" {
		// Sync the Group as a Namespaced Group.
		return n.syncInternalNamespacedGroup(grp)
	}
	return n.syncInternalClusterGroup(grp)
}
