# Known gaps

- **Bridge reads Secrets cluster-wide in the application cluster.** Before writing its connection Secret the bridge reads the object by name to refuse adopting foreign objects, so its ClusterRole carries `get` on all Secrets. The admission policy restricts writes only. Fix options: per-`dp-*` namespace Roles rendered by the operator, or a pure server-side apply flow with ownership checks done through field managers instead of a read. Found on the stand on 2026-11-25.
