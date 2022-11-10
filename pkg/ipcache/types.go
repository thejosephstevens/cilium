// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ipcache

import (
	"bytes"
	"sort"

	"github.com/sirupsen/logrus"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"

	"github.com/cilium/cilium/pkg/ipcache/types"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/source"
)

// prefixInfo holds all of the information (labels, etc.) about a given prefix
// independently based on the ResourceID of the origin of that information, and
// provides convenient accessors to consistently merge the stored information
// to generate ipcache output based on a range of inputs.
type prefixInfo map[types.ResourceID]*resourceInfo

// IdentityOverride can be used to override the identity of a given prefix.
// Must be provided together with a set of labels. Any other labels associated
// with this prefix are ignored while an override is present.
// This type implements ipcache.IPMetadata
type overrideIdentity bool

// resourceInfo is all of the information that has been collected from a given
// resource (types.ResourceID) about this IP. Each field must have a 'zero'
// value that indicates that it should be ignored for purposes of merging
// multiple resourceInfo across multiple ResourceIDs together.
type resourceInfo struct {
	labels           labels.Labels
	source           source.Source
	identityOverride overrideIdentity

	tunnelPeer types.TunnelPeer
	encryptKey types.EncryptKey
}

// IPMetadata is an empty interface intended to inform developers using the
// IPCache interface about which types are valid to be injected, and how to
// update this code, in particular the merge(),unmerge(),isValid() methods
// below.
//
// In an ideal world, we would use Constraints here but as of Go 1.18, these
// cannot be used in conjunction with methods, which is how the information
// gets injected into the IPCache.
type IPMetadata any

// merge overwrites the field in 'resourceInfo' corresponding to 'info'. This
// associates the new information with the prefix and ResourceID that this
// 'resourceInfo' resides under in the outer metadata map.
func (m *resourceInfo) merge(info IPMetadata, src source.Source) {
	switch info := info.(type) {
	case labels.Labels:
		m.labels = labels.NewFrom(info)
	case overrideIdentity:
		m.identityOverride = info
	case types.TunnelPeer:
		m.tunnelPeer = info
	case types.EncryptKey:
		m.encryptKey = info
	default:
		log.Errorf("BUG: Invalid IPMetadata passed to ipinfo.merge(): %+v", info)
		return
	}
	m.source = src
}

// unmerge removes the info of the specified type from 'resourceInfo'.
func (m *resourceInfo) unmerge(info IPMetadata) {
	switch info.(type) {
	case labels.Labels:
		m.labels = nil
	case overrideIdentity:
		m.identityOverride = false
	case types.TunnelPeer:
		m.tunnelPeer = types.TunnelPeer{}
	case types.EncryptKey:
		m.encryptKey = types.EncryptKeyEmpty
	default:
		log.Errorf("BUG: Invalid IPMetadata passed to ipinfo.unmerge(): %+v", info)
		return
	}
}

func (m *resourceInfo) isValid() bool {
	if m.labels != nil {
		return true
	}
	if m.identityOverride {
		return true
	}
	if m.tunnelPeer.IsValid() {
		return true
	}
	if m.encryptKey.IsValid() {
		return true
	}
	return false
}

func (s prefixInfo) isValid() bool {
	for _, v := range s {
		if v.isValid() {
			return true
		}
	}
	return false
}

func (s prefixInfo) sortedByResourceIDsAndSource() []types.ResourceID {
	resourceIDs := maps.Keys(s)
	slices.Sort(resourceIDs)
	slices.SortFunc(resourceIDs, func(a, b types.ResourceID) bool {
		return !source.AllowOverwrite(s[a].source, s[b].source)
	})
	return resourceIDs
}

func (s prefixInfo) ToLabels() labels.Labels {
	l := labels.NewLabelsFromModel(nil)
	for _, v := range s {
		l.MergeLabels(v.labels)
	}
	return l
}

func (s prefixInfo) Source() source.Source {
	src := source.Unspec
	for _, v := range s {
		if source.AllowOverwrite(src, v.source) {
			src = v.source
		}
	}
	return src
}

func (s prefixInfo) EncryptKey() types.EncryptKey {
	for _, resourceID := range s.sortedByResourceIDsAndSource() {
		encryptKey := s[resourceID].encryptKey
		if encryptKey.IsValid() {
			return encryptKey
		}
	}
	return types.EncryptKeyEmpty
}

func (s prefixInfo) TunnelPeer() types.TunnelPeer {
	for _, resourceID := range s.sortedByResourceIDsAndSource() {
		tunnelPeer := s[resourceID].tunnelPeer
		if tunnelPeer.IsValid() {
			return tunnelPeer
		}
	}
	return types.TunnelPeer{}
}

// identityOverride extracts the labels of the pre-determined identity from
// the prefix info. If no override identity is present, this returns nil.
// This pre-determined identity will overwrite any other identity which may
// be derived from the prefix labels.
func (s prefixInfo) identityOverride() (lbls labels.Labels, hasOverride bool) {
	identities := make([]labels.Labels, 0, 1)
	for _, info := range s {
		// We emit a warning in logConflicts if an identity override
		// was requested without labels
		if info.identityOverride && len(info.labels) > 0 {
			identities = append(identities, info.labels)
		}
	}

	// No override identity present
	if len(identities) == 0 {
		return nil, false
	}

	// Conflict-resolution: We pick the labels with the alphabetically
	// lowest value when formatted in the KV store format. The conflict
	// is logged below in logConflicts.
	if len(identities) > 1 {
		sort.Slice(identities, func(i, j int) bool {
			a := identities[i].SortedList()
			b := identities[j].SortedList()
			return bytes.Compare(a, b) == -1
		})
	}

	return identities[0], true
}

func (s prefixInfo) logConflicts(scopedLog *logrus.Entry) {
	var (
		override           labels.Labels
		overrideResourceID types.ResourceID

		tunnelPeer           types.TunnelPeer
		tunnelPeerResourceID types.ResourceID

		encryptKey           types.EncryptKey
		encryptKeyResourceID types.ResourceID
	)

	for _, resourceID := range s.sortedByResourceIDsAndSource() {
		info := s[resourceID]

		if info.identityOverride {
			if len(override) > 0 {
				scopedLog.WithFields(logrus.Fields{
					logfields.Identity:            override.String(),
					logfields.Resource:            overrideResourceID,
					logfields.ConflictingIdentity: info.labels.String(),
					logfields.ConflictingResource: resourceID,
				}).Warning("Detected conflicting identity override for prefix. " +
					"This may cause connectivity issues for this address.")
			}

			if len(info.labels) == 0 {
				scopedLog.WithFields(logrus.Fields{
					logfields.Resource:    resourceID,
					logfields.OldIdentity: s.ToLabels().String(),
				}).Warning("Detected identity override, but no labels where specified. " +
					"Falling back on the old non-override labels. " +
					"This may cause connectivity issues for this address.")
			} else {
				override = info.labels
				overrideResourceID = resourceID
			}
		}

		if info.tunnelPeer.IsValid() {
			if tunnelPeer.IsValid() {
				scopedLog.WithFields(logrus.Fields{
					logfields.TunnelPeer:            tunnelPeer.String(),
					logfields.Resource:              tunnelPeerResourceID,
					logfields.ConflictingTunnelPeer: info.tunnelPeer.String(),
					logfields.ConflictingResource:   resourceID,
				}).Warning("Detected conflicting tunnel peer for prefix. " +
					"This may cause connectivity issues for this address.")
			} else {
				tunnelPeer = info.tunnelPeer
				tunnelPeerResourceID = resourceID
			}
		}

		if info.encryptKey.IsValid() {
			if encryptKey.IsValid() {
				scopedLog.WithFields(logrus.Fields{
					logfields.Key:                 encryptKey.String(),
					logfields.Resource:            encryptKeyResourceID,
					logfields.ConflictingKey:      info.encryptKey.String(),
					logfields.ConflictingResource: resourceID,
				}).Warning("Detected conflicting encryption key index for prefix. " +
					"This may cause connectivity issues for this address.")
			} else {
				encryptKey = info.encryptKey
				encryptKeyResourceID = resourceID
			}
		}
	}
}
