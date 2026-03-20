package deploy

import _ "embed"

// NodeCheckJobYAML is the embedded per-node check Job manifest.
//
//go:embed node-check-job.yaml
var NodeCheckJobYAML []byte

// RBACYAML is the embedded RBAC manifest (ServiceAccount, ClusterRole, ClusterRoleBinding).
//
//go:embed rbac.yaml
var RBACYAML []byte
