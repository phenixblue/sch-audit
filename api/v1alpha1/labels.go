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

// ExpiresAtLabel is set on every SchedulingDecision at creation time, to the
// Unix epoch second after which the record is eligible for deletion by the
// retention sweep (cmd/sweep). It's exported because both the controller
// (which writes it) and the sweep binary (which reads it) need the exact
// same key; a label rather than an annotation so it stays usable as a
// selector if a future sweep implementation wants to filter server-side.
//
// The value is a Unix timestamp, not RFC3339, because Kubernetes label
// values can't contain colons.
const ExpiresAtLabel = "scheduling.purestorage.io/expires-at"
