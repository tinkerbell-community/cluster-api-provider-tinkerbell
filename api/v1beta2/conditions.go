/*
Copyright The Tinkerbell Authors.

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

package v1beta2

// TinkerbellMachine condition types and reasons.
const (
	// IPAddressClaimedCondition reports whether the address requested through
	// spec.addressFromPool has been allocated and written to the selected Hardware.
	// It is absent when no pool is configured.
	IPAddressClaimedCondition = "IPAddressClaimed"

	// IPAddressClaimedReason means the address is allocated and reserved on the Hardware.
	IPAddressClaimedReason = "IPAddressClaimed"

	// WaitingForIPAddressReason means the IPAddressClaim exists but the IPAM provider has not
	// bound an address to it yet.
	WaitingForIPAddressReason = "WaitingForIPAddress"

	// IPAddressClaimDeletingReason means the IPAddressClaim has a deletion timestamp. CAPT does
	// not recreate it, because the machine may be running on the address being released.
	IPAddressClaimDeletingReason = "IPAddressClaimDeleting"

	// IPAddressClaimInvalidReason means the claim or its address cannot be applied to the
	// Hardware; the condition message says why.
	IPAddressClaimInvalidReason = "IPAddressClaimInvalid"
)
