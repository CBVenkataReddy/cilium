// Copyright 2019-2020 Authors of Cilium
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

package endpoint

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/identity/cache"
	identitymodel "github.com/cilium/cilium/pkg/identity/model"
	cilium_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy"
)

func getEndpointStatusControllers(mdlControllers models.ControllerStatuses) (controllers cilium_v2.ControllerList) {
	for _, c := range mdlControllers {
		if c.Status == nil {
			continue
		}

		if c.Status.ConsecutiveFailureCount > 0 {
			s := cilium_v2.ControllerStatus{
				Configuration: c.Configuration,
				Name:          c.Name,
				UUID:          string(c.UUID),
				Status: cilium_v2.ControllerStatusStatus{
					ConsecutiveFailureCount: c.Status.ConsecutiveFailureCount,
					FailureCount:            c.Status.FailureCount,
					LastFailureMsg:          c.Status.LastFailureMsg,
					LastFailureTimestamp:    c.Status.LastFailureTimestamp.String(),
					LastSuccessTimestamp:    c.Status.LastSuccessTimestamp.String(),
					SuccessCount:            c.Status.SuccessCount,
				},
			}
			if controllers == nil {
				controllers = cilium_v2.ControllerList{s}
			} else {
				controllers = append(controllers, s)
			}
		}
	}

	if controllers != nil {
		controllers.Sort()
	}

	return
}

func (e *Endpoint) getEndpointStatusLog() (log []*models.EndpointStatusChange) {
	added := 0

	if s := e.status; s != nil {
		s.indexMU.RLock()
		defer s.indexMU.RUnlock()

		for i := s.lastIndex(); ; i-- {
			if i < 0 {
				i = maxLogs - 1
			}
			if i < len(s.Log) && s.Log[i] != nil {
				l := &models.EndpointStatusChange{
					Timestamp: s.Log[i].Timestamp.Format(time.RFC3339),
					Code:      s.Log[i].Status.Code.String(),
					Message:   s.Log[i].Status.Msg,
					State:     models.EndpointState(s.Log[i].Status.State),
				}

				if strings.ToLower(l.Code) != models.EndpointStatusChangeCodeOk {
					if log == nil {
						log = []*models.EndpointStatusChange{l}
					} else {
						log = append(log, l)
					}

					// Limit the number of endpoint log
					// entries to keep the size of the
					// EndpointStatus low.
					added++
					if added >= cilium_v2.EndpointStatusLogEntries {
						break
					}
				}
			}
			if i == s.Index {
				break
			}
		}
	}
	return
}

func getEndpointIdentity(mdlIdentity *models.Identity) (identity *cilium_v2.EndpointIdentity) {
	if mdlIdentity == nil {
		return
	}
	identity = &cilium_v2.EndpointIdentity{
		ID: mdlIdentity.ID,
	}

	identity.Labels = make([]string, len(mdlIdentity.Labels))
	copy(identity.Labels, mdlIdentity.Labels)
	sort.Strings(identity.Labels)
	return
}

func getEndpointNetworking(mdlNetworking *models.EndpointNetworking) (networking *cilium_v2.EndpointNetworking) {
	if mdlNetworking == nil {
		return nil
	}
	networking = &cilium_v2.EndpointNetworking{
		Addressing: make(cilium_v2.AddressPairList, len(mdlNetworking.Addressing)),
	}

	if option.Config.EnableIPv4 {
		networking.NodeIP = node.GetExternalIPv4().String()
	} else {
		networking.NodeIP = node.GetIPv6().String()
	}

	for i, pair := range mdlNetworking.Addressing {
		networking.Addressing[i] = &cilium_v2.AddressPair{
			IPV4: pair.IPV4,
			IPV6: pair.IPV6,
		}
	}

	networking.Addressing.Sort()
	return
}

// updateLabels inserts the labels correnspoding to the specified identity into
// the AllowedIdentityTuple.
func updateLabels(allocator cache.IdentityAllocator, allowedIdentityTuple *cilium_v2.AllowedIdentityTuple, secID identity.NumericIdentity) {
	// IdentityUnknown denotes that this is an L4-only BPF
	// allow, so it applies to all identities. In this case
	// we should skip resolving the labels, because the
	// value 0 does not denote an allow for the "unknown"
	// identity, but instead an allow of all identities for
	// that port.
	if secID != identity.IdentityUnknown {
		identity := allocator.LookupIdentityByID(context.TODO(), secID)
		if identity != nil {
			var l labels.Labels
			if identity.CIDRLabel != nil {
				l = identity.CIDRLabel
			} else {
				l = identity.Labels
			}

			allowedIdentityTuple.IdentityLabels = l.StringMap()
		}
	}
}

// populateResponseWithPolicyKey inserts an AllowedIdentityTuple element into 'policy'
// which corresponds to the specified 'desiredPolicy'.
func populateResponseWithPolicyKey(allocator cache.IdentityAllocator, policy *cilium_v2.EndpointPolicy, policyKey *policy.Key) {
	allowedIdentityTuple := cilium_v2.AllowedIdentityTuple{
		DestPort: policyKey.DestPort,
		Protocol: policyKey.Nexthdr,
		Identity: uint64(policyKey.Identity),
	}

	secID := identity.NumericIdentity(policyKey.Identity)
	updateLabels(allocator, &allowedIdentityTuple, secID)

	switch {
	case policyKey.IsIngress():
		if policy.Ingress.Allowed == nil {
			policy.Ingress.Allowed = cilium_v2.AllowedIdentityList{allowedIdentityTuple}
		} else {
			policy.Ingress.Allowed = append(policy.Ingress.Allowed, allowedIdentityTuple)
		}
	case policyKey.IsEgress():
		if policy.Egress.Allowed == nil {
			policy.Egress.Allowed = cilium_v2.AllowedIdentityList{allowedIdentityTuple}
		} else {
			policy.Egress.Allowed = append(policy.Egress.Allowed, allowedIdentityTuple)
		}
	}
}

// getEndpointPolicy returns an API representation of the policy that the
// received Endpoint intends to apply.
func (e *Endpoint) getEndpointPolicy() (policy *cilium_v2.EndpointPolicy) {
	if e.desiredPolicy != nil {
		policy = &cilium_v2.EndpointPolicy{
			Ingress: &cilium_v2.EndpointPolicyDirection{
				Enforcing: !e.Options.IsEnabled(option.PolicyAuditMode) && e.desiredPolicy.IngressPolicyEnabled,
			},
			Egress: &cilium_v2.EndpointPolicyDirection{
				Enforcing: !e.Options.IsEnabled(option.PolicyAuditMode) && e.desiredPolicy.EgressPolicyEnabled,
			},
		}

		// Handle allow-all cases
		allowsAllIngress, allowsAllEgress := e.desiredPolicy.AllowsIdentity(identity.IdentityUnknown)
		if allowsAllIngress {
			policy.Ingress.Allowed = cilium_v2.AllowedIdentityList{{}}
		}
		if allowsAllEgress {
			policy.Egress.Allowed = cilium_v2.AllowedIdentityList{{}}
		}

		// If either ingress or egress policy is enabled, go through
		// the desired policy to populate the values.
		if !allowsAllIngress || !allowsAllEgress {
			allowsWorldIngress, allowsWorldEgress := e.desiredPolicy.AllowsIdentity(identity.ReservedIdentityWorld)

			for policyKey := range e.desiredPolicy.PolicyMapState {
				// Skip listing identities if enforcement is disabled in direction,
				// or if the identity corresponds to a CIDR identity and the world is allowed.
				id := identity.NumericIdentity(policyKey.Identity)
				switch {
				case policyKey.IsIngress():
					if allowsAllIngress || (id.HasLocalScope() && allowsWorldIngress) {
						continue
					}
				case policyKey.IsEgress():
					if allowsAllEgress || (id.HasLocalScope() && allowsWorldEgress) {
						continue
					}
				}

				populateResponseWithPolicyKey(e.allocator, policy, &policyKey)
			}
		}

		if policy.Ingress.Allowed != nil {
			policy.Ingress.Allowed.Sort()
		}
		if policy.Egress.Allowed != nil {
			policy.Egress.Allowed.Sort()
		}
	}

	return
}

func (e *Endpoint) getEndpointVisibilityPolicyStatus() *string {
	if e.visibilityPolicy == nil {
		return nil
	}
	var str string
	if e.visibilityPolicy.Error == nil {
		str = "OK"
	} else {
		str = e.visibilityPolicy.Error.Error()
	}
	return &str
}

// EndpointStatusConfiguration is the configuration interface that a caller of
// to GetCiliumEndpointStatus() must implement
type EndpointStatusConfiguration interface {
	// EndpointStatusIsEnabled must return true if a particular
	// option.EndpointStatus* feature is enabled
	EndpointStatusIsEnabled(option string) bool
}

func compressEndpointState(state models.EndpointState) string {
	switch state {
	case models.EndpointStateRestoring, models.EndpointStateWaitingToRegenerate,
		models.EndpointStateRegenerating, models.EndpointStateReady,
		models.EndpointStateDisconnecting, models.EndpointStateDisconnected:
		return string(models.EndpointStateReady)
	}

	return string(state)
}

// GetCiliumEndpointStatus creates a cilium_v2.EndpointStatus of an endpoint.
// See cilium_v2.EndpointStatus for a detailed explanation of each field.
func (e *Endpoint) GetCiliumEndpointStatus(conf EndpointStatusConfiguration) *cilium_v2.EndpointStatus {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	status := &cilium_v2.EndpointStatus{
		ID:                  int64(e.ID),
		ExternalIdentifiers: e.getModelEndpointIdentitiersRLocked(),
		Identity:            getEndpointIdentity(identitymodel.CreateModel(e.SecurityIdentity)),
		Networking:          getEndpointNetworking(e.getModelNetworkingRLocked()),
		State:               compressEndpointState(e.getModelCurrentStateRLocked()),
		Encryption:          cilium_v2.EncryptionSpec{Key: int(node.GetIPsecKeyIdentity())},
		NamedPorts:          e.getNamedPortsModel(),
	}

	if conf.EndpointStatusIsEnabled(option.EndpointStatusControllers) {
		controllerMdl := e.controllers.GetStatusModel()
		status.Controllers = getEndpointStatusControllers(controllerMdl)
	}

	if conf.EndpointStatusIsEnabled(option.EndpointStatusPolicy) {
		status.Policy = e.getEndpointPolicy()
		status.VisibilityPolicyStatus = e.getEndpointVisibilityPolicyStatus()
	}

	if conf.EndpointStatusIsEnabled(option.EndpointStatusHealth) {
		status.Health = e.getHealthModel()
	}

	if conf.EndpointStatusIsEnabled(option.EndpointStatusLog) {
		status.Log = e.getEndpointStatusLog()
	}

	if conf.EndpointStatusIsEnabled(option.EndpointStatusState) {
		status.State = compressEndpointState(e.getModelCurrentStateRLocked())
	}

	return status
}
