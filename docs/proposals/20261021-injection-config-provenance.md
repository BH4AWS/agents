# Preserve Injection Configuration Sources

## Background
`sidecarutils` reads injection templates from `sandbox-injection-config` and
an optional `sandbox-injection-config-user`. User configuration overrides only
matching keys; other keys continue to use the default configuration. The current
loader merges both sources immediately, so downstream code cannot distinguish
explicit user overrides from defaults.

This proposal preserves source information without changing template selection,
injection order, configuration formats, or error handling.

## Data Model
Define the following type in `pkg/utils/sidecarutils/config_types.go`:

```go
package sidecarutils

type InjectionConfigs struct {
    Default map[string]string
    User    map[string]string
}
```

Each source field has the following semantics:
- `nil`: the corresponding ConfigMap was not found.
- An empty, non-nil map: the ConfigMap exists but contains no data.
- A non-empty map: the ConfigMap exists, with all its key/value entries preserved.

A successful read with `ConfigMap.Data == nil` is normalized to an empty,
non-nil map. Loaded data is copied into new maps without modifying objects held
by the client or informer. Callers can detect explicit overrides by checking
whether a key exists in `User`; an empty string also counts as an override.

## Loading and Merging
`InjectionConfigs.Merged()` returns an independent, non-nil map by copying
`Default` first and then `User`. Its result is identical to the existing merge:

| Default configuration | User configuration | Merged result |
|---|---|---|
| `csi: default` | No `csi` key | `csi: default` |
| `csi: default` | `csi: custom` | `csi: custom` |
| `csi: default` | `csi: ""` | `csi: ""` |
| Absent | `csi: custom` | `csi: custom` |
| Both sources absent or empty | — | Empty, non-nil map |

`fetchInjectionConfiguration` returns `InjectionConfigs`. Read order,
namespace selection, NotFound handling, and propagation of other lookup errors
remain unchanged. A failed read returns the zero value and the original error,
without passing partial configuration to the injection flow.

`InjectSandboxRuntimes` passes the result of `Merged()` to the existing
`doSidecarInjection`. The latter's signature, parsing logic, and execution order
remain unchanged. The exported injection entrypoint keeps its signature,
so callers need no changes.

## Compatibility and Non-Goals
- Do not change `SidecarInjectConfig` or the JSON/YAML formats of either ConfigMap.
- Do not introduce new runtimes, topology selection, probes, feature gates, or
  controller behavior.
- Do not add ConfigMap reads or Kubernetes API writes.
- Missing templates, empty user overrides, and invalid templates remain subject
  to the existing parsing logic.
- Preserve the distinction between absent and present-but-empty sources without
  changing the behavior of the community injection entrypoint.
- Each `Merged()` call returns a new map; mutating it does not affect either
  source or other merge results.

## Validation
Extend the existing configuration-loading tests to cover defaults, user
overrides, user-only configuration, both sources absent, nil data, empty data,
empty user overrides, and lookup failures.

Verify that the retained sources match the original ConfigMaps. Mutating a
returned source or merge result must not affect subsequent ConfigMap reads or
independent merges. Existing injection tests must continue to pass.
