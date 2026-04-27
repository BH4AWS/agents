/*
Copyright 2025.

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

package identityprovider

// init overrides the community default (UUID) provider with the internal secure provider.
// This file is only present in internal deployments — community builds do not include it,
// so DefaultProvider remains the UUID-based no-op provider.
//
// Initialization order within this init():
//  1. Register internal propagators (e.g., WriteSecurityTokenToRuntime) into the global registry.
//  2. Call initSecureProvider() which copies the registered propagators into secureIdentityProvider.
//
// Note: SetRunCommandFunc is injected later by utils/runtime init(). This is safe because
// WriteSecurityTokenToRuntime checks runCommandFunc at execution time, not at registration time.
func init() {
	// Step 1: Register internal security token propagators.
	RegisterSecurityTokenPropagator(WriteSecurityTokenToRuntime)

	// Step 2: Create and register the secure identity provider with all propagators.
	initSecureProvider()
}
