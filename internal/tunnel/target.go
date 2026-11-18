package tunnel

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TargetType is the canonical Kubernetes resource type of a tunnel target.
type TargetType string

// Supported canonical target types.
const (
	TargetPod                   TargetType = "pod"
	TargetService               TargetType = "service"
	TargetDeployment            TargetType = "deployment"
	TargetReplicaSet            TargetType = "replicaset"
	TargetStatefulSet           TargetType = "statefulset"
	TargetDaemonSet             TargetType = "daemonset"
	TargetJob                   TargetType = "job"
	TargetReplicationController TargetType = "replicationcontroller"
)

// supportedTargetTypes lists the accepted canonical target types in a stable
// order for diagnostics.
var supportedTargetTypes = []TargetType{
	TargetPod,
	TargetService,
	TargetDeployment,
	TargetReplicaSet,
	TargetStatefulSet,
	TargetDaemonSet,
	TargetJob,
	TargetReplicationController,
}

// targetAliases maps every accepted spelling to its canonical type. Only
// built-in workload resources are supported; group/version-qualified names and
// custom resources are deliberately absent.
var targetAliases = map[string]TargetType{
	"pod":                    TargetPod,
	"pods":                   TargetPod,
	"po":                     TargetPod,
	"service":                TargetService,
	"services":               TargetService,
	"svc":                    TargetService,
	"deployment":             TargetDeployment,
	"deployments":            TargetDeployment,
	"deploy":                 TargetDeployment,
	"replicaset":             TargetReplicaSet,
	"replicasets":            TargetReplicaSet,
	"rs":                     TargetReplicaSet,
	"statefulset":            TargetStatefulSet,
	"statefulsets":           TargetStatefulSet,
	"sts":                    TargetStatefulSet,
	"daemonset":              TargetDaemonSet,
	"daemonsets":             TargetDaemonSet,
	"ds":                     TargetDaemonSet,
	"job":                    TargetJob,
	"jobs":                   TargetJob,
	"replicationcontroller":  TargetReplicationController,
	"replicationcontrollers": TargetReplicationController,
	"rc":                     TargetReplicationController,
}

// Target identifies where a tunnel's backing pods come from. A bare name means
// pod/<name>; otherwise the input is TYPE/NAME with TYPE resolved through
// targetAliases.
type Target struct {
	Type TargetType
	Name string
}

// Canonical returns the target-scoped identity spelling used for tunnel keys
// and data source IDs, for example "service/postgres".
func (t Target) Canonical() string {
	return string(t.Type) + "/" + t.Name
}

func (t Target) String() string {
	return t.Canonical()
}

// ParseTarget parses a user-supplied target. It accepts a bare pod name or
// TYPE/NAME, where TYPE is a built-in resource type or one of the common
// kubectl aliases. Empty segments, extra slashes, and unknown types are
// rejected with actionable diagnostics.
func ParseTarget(raw string) (Target, error) {
	if raw == "" {
		return Target{}, fmt.Errorf("target must not be empty; use TYPE/NAME such as pod/my-pod, or a bare pod name")
	}
	if strings.TrimSpace(raw) != raw {
		return Target{}, fmt.Errorf("target %q must not contain leading or trailing whitespace", raw)
	}

	parts := strings.Split(raw, "/")
	switch len(parts) {
	case 1:
		name := parts[0]
		if name == "" {
			return Target{}, fmt.Errorf("target %q is empty; use TYPE/NAME such as pod/my-pod, or a bare pod name", raw)
		}
		if err := validateTargetName(name); err != nil {
			return Target{}, fmt.Errorf("invalid target %q: %w", raw, err)
		}
		return Target{Type: TargetPod, Name: name}, nil
	case 2:
		typeName, name := parts[0], parts[1]
		if typeName == "" {
			return Target{}, fmt.Errorf("target %q has an empty type segment; use TYPE/NAME such as pod/my-pod", raw)
		}
		if name == "" {
			return Target{}, fmt.Errorf("target %q has an empty name segment; use TYPE/NAME such as pod/my-pod", raw)
		}
		targetType, ok := targetAliases[typeName]
		if !ok {
			return Target{}, fmt.Errorf(
				"unsupported target type %q; supported types are %s (plus common kubectl aliases such as po, svc, deploy, rs, sts, ds, rc); custom resources and group/version-qualified names are not supported",
				typeName, strings.Join(supportedTypeNames(), ", "),
			)
		}
		if err := validateTargetName(name); err != nil {
			return Target{}, fmt.Errorf("invalid target %q: %w", raw, err)
		}
		return Target{Type: targetType, Name: name}, nil
	default:
		return Target{}, fmt.Errorf(
			"target %q contains more than one '/'; use TYPE/NAME such as pod/my-pod, or a bare pod name (group/version-qualified names and subresources are not supported)",
			raw,
		)
	}
}

func supportedTypeNames() []string {
	names := make([]string, 0, len(supportedTargetTypes))
	for _, t := range supportedTargetTypes {
		names = append(names, string(t))
	}
	return names
}

func validateTargetName(name string) error {
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("name %q is not a valid Kubernetes resource name: %s", name, strings.Join(errs, "; "))
	}
	return nil
}
