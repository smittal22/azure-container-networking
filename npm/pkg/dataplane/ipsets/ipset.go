package ipsets

import (
	"errors"
	"fmt"

	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/util"
)

type IPSetMetadata struct {
	Name string
	Type SetType
}

type SetKind string

const (
	// ListSet is of kind list with members as other IPSets
	ListSet SetKind = "list"
	// HashSet is of kind hashset with members as IPs and/or port
	HashSet SetKind = "set"
	// UnknownKind is returned when kind is unknown
	UnknownKind SetKind = "unknown"
)

// NewIPSetMetadata is used for controllers to send in skeleton ipsets to DP
func NewIPSetMetadata(name string, setType SetType) *IPSetMetadata {
	set := &IPSetMetadata{
		Name: name,
		Type: setType,
	}
	return set
}

func (setMetadata *IPSetMetadata) GetHashedName() string {
	prefixedName := setMetadata.GetPrefixName()
	if prefixedName == Unknown {
		return Unknown
	}
	return util.GetHashedName(prefixedName)
}

// TODO join with colon instead of dash for easier readability?
func (setMetadata *IPSetMetadata) GetPrefixName() string {
	switch setMetadata.Type {
	case CIDRBlocks:
		return fmt.Sprintf("%s%s", util.CIDRPrefix, setMetadata.Name)
	case Namespace:
		return fmt.Sprintf("%s%s", util.NamespacePrefix, setMetadata.Name)
	case NamedPorts:
		return fmt.Sprintf("%s%s", util.NamedPortIPSetPrefix, setMetadata.Name)
	case KeyLabelOfPod:
		return fmt.Sprintf("%s%s", util.PodLabelPrefix, setMetadata.Name)
	case KeyValueLabelOfPod:
		return fmt.Sprintf("%s%s", util.PodLabelPrefix, setMetadata.Name)
	case KeyLabelOfNamespace:
		return fmt.Sprintf("%s%s", util.NamespaceLabelPrefix, setMetadata.Name)
	case KeyValueLabelOfNamespace:
		return fmt.Sprintf("%s%s", util.NamespaceLabelPrefix, setMetadata.Name)
	case NestedLabelOfPod:
		return fmt.Sprintf("%s%s", util.NestedLabelPrefix, setMetadata.Name)
	case EmptyHashSet:
		return fmt.Sprintf("%s%s", util.EmptySetPrefix, setMetadata.Name)
	case UnknownType: // adding this to appease golint
		metrics.SendErrorLogAndMetric(util.UtilID, "experienced unknown type in set metadata: %+v", setMetadata)
		return Unknown
	default:
		metrics.SendErrorLogAndMetric(util.UtilID, "experienced unexpected type %d in set metadata: %+v", setMetadata.Type, setMetadata)
		return Unknown
	}
}

func (setMetadata *IPSetMetadata) GetSetKind() SetKind {
	return setMetadata.Type.getSetKind()
}

func (setType SetType) getSetKind() SetKind {
	switch setType {
	case CIDRBlocks:
		return HashSet
	case Namespace:
		return HashSet
	case NamedPorts:
		return HashSet
	case KeyLabelOfPod:
		return HashSet
	case KeyValueLabelOfPod:
		return HashSet
	case EmptyHashSet:
		return HashSet
	case KeyLabelOfNamespace:
		return ListSet
	case KeyValueLabelOfNamespace:
		return ListSet
	case NestedLabelOfPod:
		return ListSet
	case UnknownType: // adding this to appease golint
		return UnknownKind
	default:
		return UnknownKind
	}
}

// TranslatedIPSet is created by translation engine and provides IPSets used in
// network policy. Only 2 types of IPSets are generated with members:
// 1. CIDRBlocks IPSet
// 2. NestedLabelOfPod IPSet from multi value labels
// Members field holds member ipset names for NestedLabelOfPod and ip address ranges
// for CIDRBlocks IPSet
// Caveat: if a list set with translated members is referenced in multiple policies,
// then it must have a different ipset name for each policy. Otherwise, deleting the policy
// will result in removing the translated members from the set even if another policy requires
// those members. See dataplane.go for more details.
type TranslatedIPSet struct {
	Metadata *IPSetMetadata
	// Members holds member ipset names for NestedLabelOfPod and ip address ranges
	// for CIDRBlocks IPSet
	Members []string
}

// NewTranslatedIPSet creates TranslatedIPSet.
// Only nested labels from podSelector and IPBlock has members and others has nil slice.
func NewTranslatedIPSet(name string, setType SetType, members ...string) *TranslatedIPSet {
	translatedIPSet := &TranslatedIPSet{
		Metadata: NewIPSetMetadata(name, setType),
		Members:  members,
	}
	return translatedIPSet
}

type SetProperties struct {
	// Stores type of ip grouping
	Type SetType
	// Stores kind of ipset in dataplane
	Kind SetKind
}

type SetType int8

// Possble values for SetType
const (
	// Unknown SetType
	UnknownType SetType = 0
	// Namespace IPSet is created to hold
	// ips of pods in a given NameSapce
	Namespace SetType = 1
	// KeyLabelOfNamespace IPSet is a list kind ipset
	// with members as ipsets of namespace with this Label Key
	KeyLabelOfNamespace SetType = 2
	// KeyValueLabelOfNamespace IPSet is a list kind ipset
	// with members as ipsets of namespace with this Label
	KeyValueLabelOfNamespace SetType = 3
	// KeyLabelOfPod IPSet contains IPs of Pods with this Label Key
	KeyLabelOfPod SetType = 4
	// KeyValueLabelOfPod IPSet contains IPs of Pods with this Label
	KeyValueLabelOfPod SetType = 5
	// NamedPorts IPSets contains a given namedport
	NamedPorts SetType = 6
	// NestedLabelOfPod is derived for multivalue matchexpressions
	NestedLabelOfPod SetType = 7
	// CIDRBlocks holds CIDR blocks
	CIDRBlocks SetType = 8
	// EmptyHashSet is a set meant to have no members
	EmptyHashSet SetType = 9

	// Unknown const for unknown string
	Unknown string = "unknown"
)

var (
	setTypeName = map[SetType]string{
		UnknownType:              Unknown,
		Namespace:                "Namespace",
		KeyLabelOfNamespace:      "KeyLabelOfNamespace",
		KeyValueLabelOfNamespace: "KeyValueLabelOfNamespace",
		KeyLabelOfPod:            "KeyLabelOfPod",
		KeyValueLabelOfPod:       "KeyValueLabelOfPod",
		NamedPorts:               "NamedPorts",
		NestedLabelOfPod:         "NestedLabelOfPod",
		CIDRBlocks:               "CIDRBlocks",
		EmptyHashSet:             "EmptySet",
	}
	// ErrIPSetInvalidKind is returned when IPSet kind is invalid
	ErrIPSetInvalidKind = errors.New("invalid IPSet Kind")
)

func (x SetType) String() string {
	return setTypeName[x]
}

// ReferenceType specifies the kind of reference for an IPSet
type ReferenceType string

// Possible ReferenceTypes
const (
	SelectorType ReferenceType = "Selector"
	NetPolType   ReferenceType = "NetPol"
)

type IPSet struct {
	// Name is prefixed name of original set
	Name           string
	unprefixedName string
	// HashedName is AzureNpmPrefix (azure-npm-) + hash of prefixed name
	HashedName string
	// SetProperties embedding set properties
	SetProperties
	// IpPodKey is used for setMaps to store Ips and ports as keys
	// and podKey as value
	IPPodKey map[string]string
	// This is used for listMaps to store child IP Sets
	MemberIPSets map[string]*IPSet
	// Using a map to emulate set and value as struct{} for
	// minimal memory consumption
	// SelectorReference holds networkpolicy names where this IPSet
	// is being used in PodSelector and Namespace
	SelectorReference map[string]struct{}
	// NetPolReference holds networkpolicy names where this IPSet
	// is being referred as part of rules
	NetPolReference map[string]struct{}
	// ipsetReferCount keeps track of how many lists in the cache refer to this ipset
	ipsetReferCount int
	// kernelReferCount keeps track of how many lists in the kernel refer to this ipset
	kernelReferCount int
}

func NewIPSet(setMetadata *IPSetMetadata) *IPSet {
	prefixedName := setMetadata.GetPrefixName()
	set := &IPSet{
		Name:           prefixedName,
		unprefixedName: setMetadata.Name,
		HashedName:     util.GetHashedName(prefixedName),
		SetProperties: SetProperties{
			Type: setMetadata.Type,
			Kind: setMetadata.GetSetKind(),
		},
		// Map with Key as Network Policy name to to emulate set
		// and value as struct{} for minimal memory consumption
		SelectorReference: make(map[string]struct{}),
		// Map with Key as Network Policy name to to emulate set
		// and value as struct{} for minimal memory consumption
		NetPolReference:  make(map[string]struct{}),
		ipsetReferCount:  0,
		kernelReferCount: 0,
	}
	if set.Kind == HashSet {
		set.IPPodKey = make(map[string]string)
	} else {
		set.MemberIPSets = make(map[string]*IPSet)
	}
	return set
}

// GetSetMetadata returns set metadata with unprefixed original name and SetType
func (set *IPSet) GetSetMetadata() *IPSetMetadata {
	return NewIPSetMetadata(set.unprefixedName, set.Type)
}

func (set *IPSet) PrettyString() string {
	return fmt.Sprintf("Name: %s HashedNamed: %s Type: %s Kind: %s",
		set.Name, set.HashedName, setTypeName[set.Type], string(set.Kind))
}

// GetSetContents returns members of set as string slice
func (set *IPSet) GetSetContents() ([]string, error) {
	switch set.Kind {
	case HashSet:
		i := 0
		contents := make([]string, len(set.IPPodKey))
		for podIP := range set.IPPodKey {
			contents[i] = podIP
			i++
		}
		return contents, nil
	case ListSet:
		i := 0
		contents := make([]string, len(set.MemberIPSets))
		for _, memberSet := range set.MemberIPSets {
			contents[i] = memberSet.HashedName
			i++
		}
		return contents, nil
	default:
		return []string{}, ErrIPSetInvalidKind
	}
}

// ShallowCompare check if the properties of IPSets are same
func (set *IPSet) ShallowCompare(newSet *IPSet) bool {
	if set.Name != newSet.Name {
		return false
	}
	if set.Kind != newSet.Kind {
		return false
	}
	if set.Type != newSet.Type {
		return false
	}
	return true
}

func (set *IPSet) incIPSetReferCount() {
	set.ipsetReferCount++
}

func (set *IPSet) decIPSetReferCount() {
	if set.ipsetReferCount == 0 {
		return
	}
	set.ipsetReferCount--
}

func (set *IPSet) incKernelReferCount() {
	set.kernelReferCount++
}

func (set *IPSet) decKernelReferCount() {
	if set.kernelReferCount == 0 {
		return
	}
	set.kernelReferCount--
}

func (set *IPSet) addReference(referenceName string, referenceType ReferenceType) {
	switch referenceType {
	case SelectorType:
		set.SelectorReference[referenceName] = struct{}{}
	case NetPolType:
		set.NetPolReference[referenceName] = struct{}{}
	default:
		log.Logf("IPSet_addReference: encountered unknown ReferenceType")
	}
}

func (set *IPSet) deleteReference(referenceName string, referenceType ReferenceType) {
	switch referenceType {
	case SelectorType:
		delete(set.SelectorReference, referenceName)
	case NetPolType:
		delete(set.NetPolReference, referenceName)
	default:
		log.Logf("IPSet_deleteReference: encountered unknown ReferenceType")
	}
}

func (set *IPSet) shouldBeInKernel() bool {
	return set.usedByNetPol() || set.referencedInKernel()
}

func (set *IPSet) canBeForceDeleted() bool {
	return !set.usedByNetPol() &&
		!set.referencedInList()
}

func (set *IPSet) canBeDeleted(ignorableMember *IPSet) bool {
	var firstMember *IPSet
	for _, member := range set.MemberIPSets {
		firstMember = member
		break
	}
	listMembersOk := len(set.MemberIPSets) == 0 || (len(set.MemberIPSets) == 1 && firstMember == ignorableMember)

	return !set.usedByNetPol() &&
		!set.referencedInList() &&
		len(set.IPPodKey) == 0 &&
		listMembersOk
}

// usedByNetPol check if an IPSet is referred in network policies.
func (set *IPSet) usedByNetPol() bool {
	return len(set.SelectorReference) > 0 ||
		len(set.NetPolReference) > 0
}

func (set *IPSet) referencedInList() bool {
	return set.ipsetReferCount > 0
}

func (set *IPSet) referencedInKernel() bool {
	return set.kernelReferCount > 0
}

// panics if set is not a list set
func (set *IPSet) hasMember(memberName string) bool {
	_, isMember := set.MemberIPSets[memberName]
	return isMember
}

func (set *IPSet) canSetBeSelectorIPSet() bool {
	return (set.Type == KeyLabelOfPod ||
		set.Type == KeyValueLabelOfPod ||
		set.Type == Namespace ||
		set.Type == NestedLabelOfPod)
}

func GetMembersOfTranslatedSets(members []string) []*IPSetMetadata {
	memberList := make([]*IPSetMetadata, len(members))
	i := 0
	for _, setName := range members {
		// translate engine only returns KeyValueLabelOfPod as member
		memberSet := NewIPSetMetadata(setName, KeyValueLabelOfPod)
		memberList[i] = memberSet
		i++
	}
	return memberList
}
