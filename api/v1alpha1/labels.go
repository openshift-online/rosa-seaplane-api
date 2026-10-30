/*
Copyright 2026.

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

package v1alpha1

const (
	// AccountIDLabel records the AWS account that owns an account-scoped resource.
	AccountIDLabel = "seaplane.io/account-id"

	// ClusterUIDLabel records the database UID of the owning Cluster on
	// Cluster-owned resources such as NodePools and Placements. Keep this value
	// in sync with seaplane-db.ClusterUIDLabel, which is used for shard routing.
	ClusterUIDLabel = "seaplane.io/cluster-uid"

	// ClaimedByClusterUIDLabel records the Cluster UID holding an account-level
	// claim such as a DNSReservation or OidcConfig.
	ClaimedByClusterUIDLabel = "seaplane.io/claimed-by-cluster-uid"

	// OwnerUIDLabel records the UID that owns an internal Index claim.
	OwnerUIDLabel = "seaplane.io/owner-uid"
)
