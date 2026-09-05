package rbac

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

var k8sNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Options controls the Kubernetes deploy RBAC manifest generator.
type Options struct {
	Name                    string
	Namespace               string
	ServiceAccount          string
	ServiceAccountNamespace string
	ClusterWide             bool
	IncludeSecrets          bool
}

// RenderDeployRBAC renders a starter ServiceAccount + Role/Binding manifest for
// deploy jobs that use `cluster:`. It never applies anything; operators review and
// run kubectl themselves.
func RenderDeployRBAC(in Options) (string, error) {
	o := Options{
		Name:                    strings.TrimSpace(in.Name),
		Namespace:               strings.TrimSpace(in.Namespace),
		ServiceAccount:          strings.TrimSpace(in.ServiceAccount),
		ServiceAccountNamespace: strings.TrimSpace(in.ServiceAccountNamespace),
		ClusterWide:             in.ClusterWide,
		IncludeSecrets:          in.IncludeSecrets,
	}
	if o.Name == "" {
		o.Name = "gocdnext-deployer"
	}
	if o.ServiceAccount == "" {
		o.ServiceAccount = o.Name
	}
	if o.ServiceAccountNamespace == "" {
		o.ServiceAccountNamespace = "gocdnext-deploy"
	}
	if !o.ClusterWide && o.Namespace == "" {
		return "", fmt.Errorf("--namespace is required unless --cluster-wide is set")
	}
	if err := validateK8sName("--name", o.Name); err != nil {
		return "", err
	}
	if err := validateK8sName("--service-account", o.ServiceAccount); err != nil {
		return "", err
	}
	if err := validateK8sName("--service-account-namespace", o.ServiceAccountNamespace); err != nil {
		return "", err
	}
	if o.Namespace != "" {
		if err := validateK8sName("--namespace", o.Namespace); err != nil {
			return "", err
		}
	}

	tmpl := namespacedDeployRBACTmpl
	if o.ClusterWide {
		tmpl = clusterWideDeployRBACTmpl
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, o); err != nil {
		return "", err
	}
	return b.String(), nil
}

func validateK8sName(flag, value string) error {
	if len(value) > 63 || !k8sNameRE.MatchString(value) {
		return fmt.Errorf("%s must be a Kubernetes DNS label (lowercase alphanumeric plus '-', max 63 chars)", flag)
	}
	return nil
}

var namespacedDeployRBACTmpl = template.Must(template.New("namespaced").Parse(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{.ServiceAccount}}
  namespace: {{.ServiceAccountNamespace}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
rules:
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "daemonsets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: [{{if .IncludeSecrets}}"services", "configmaps", "secrets"{{else}}"services", "configmaps"{{end}}]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
subjects:
  - kind: ServiceAccount
    name: {{.ServiceAccount}}
    namespace: {{.ServiceAccountNamespace}}
roleRef:
  kind: Role
  name: {{.Name}}
  apiGroup: rbac.authorization.k8s.io
`))

var clusterWideDeployRBACTmpl = template.Must(template.New("cluster-wide").Parse(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{.ServiceAccount}}
  namespace: {{.ServiceAccountNamespace}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{.Name}}
rules:
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "daemonsets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: [{{if .IncludeSecrets}}"services", "configmaps", "secrets"{{else}}"services", "configmaps"{{end}}]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{.Name}}
subjects:
  - kind: ServiceAccount
    name: {{.ServiceAccount}}
    namespace: {{.ServiceAccountNamespace}}
roleRef:
  kind: ClusterRole
  name: {{.Name}}
  apiGroup: rbac.authorization.k8s.io
`))
