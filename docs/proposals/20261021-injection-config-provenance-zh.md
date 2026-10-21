# 注入配置来源保留方案

## 背景
`sidecarutils` 从 `sandbox-injection-config` 和可选的
`sandbox-injection-config-user` 读取注入模板。用户配置只覆盖同名 key，
未覆盖的 key 继续使用系统配置。现有读取函数立即合并两份数据，
后续代码无法区分用户显式配置的 key 与系统提供的 key。

本方案只保留来源，不改变模板选择、注入顺序、配置格式或错误处理。

## 数据结构
在 `pkg/utils/sidecarutils/config_types.go` 定义：

```go
package sidecarutils

type InjectionConfigs struct {
    Default map[string]string
    User    map[string]string
}
```

每个字段的状态含义：
- `nil`：对应 ConfigMap 不存在。
- 非 nil 的空 map：对应 ConfigMap 存在，但没有 data。
- 非空 map：对应 ConfigMap 存在，并保留其全部 key/value。

读取成功但 `ConfigMap.Data == nil` 时归一化为非 nil 的空 map。
读取的数据复制到新的 map，不修改客户端或 informer 持有的对象。
调用方检查 `User` 中的 key 是否存在即可识别显式覆盖；空字符串也算覆盖。

## 合并与读取
`InjectionConfigs.Merged()` 返回独立的非 nil map：先复制 `Default`，
再复制 `User`。这与现有合并结果完全相同：

| 系统配置 | 用户配置 | 合并结果 |
|---|---|---|
| `csi: default` | 无 `csi` | `csi: default` |
| `csi: default` | `csi: custom` | `csi: custom` |
| `csi: default` | `csi: ""` | `csi: ""` |
| 不存在 | `csi: custom` | `csi: custom` |
| 两份都不存在或为空 | — | 非 nil 的空 map |

`fetchInjectionConfiguration` 返回 `InjectionConfigs`。读取顺序、
命名空间、NotFound 处理和其他读取错误的传播保持不变。
读取失败时返回零值和原始错误，不向注入流程提供部分配置。

`InjectSandboxRuntimes` 将 `Merged()` 的结果交给原有
`doSidecarInjection`；后者的签名、解析逻辑和调用顺序保持不变。
既有导出的注入入口不更改签名，调用方无需改动。

## 兼容性与非目标
- 不修改 `SidecarInjectConfig` 或两个 ConfigMap 的 JSON/YAML 协议。
- 不引入新 runtime、拓扑选择、探针、feature gate 或控制器行为。
- 不新增 ConfigMap 读取或 Kubernetes API 写入。
- 缺少模板、用户空值覆盖及非法模板仍由既有解析逻辑处理。
- 保留不存在与存在但为空的差别，但不据此改变社区注入入口的行为。
- 每次 `Merged()` 都返回新的 map，修改它不会污染来源或其他合并结果。

## 验证
复用现有配置读取测试，覆盖系统配置、用户覆盖、用户独立配置、
两份缺失、nil data、空 data、用户空值覆盖及读取失败。

验证保留的来源与原 ConfigMap 一致；修改返回的来源或合并结果后，
再次读取 ConfigMap 及再次合并均不受影响。既有注入测试保持通过。
