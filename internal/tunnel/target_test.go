package tunnel

import (
	"strings"
	"testing"
)

func TestParseTargetValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input     string
		wantType  TargetType
		wantName  string
		canonical string
	}{
		{"mypod", TargetPod, "mypod", "pod/mypod"},
		{"pod/mypod", TargetPod, "mypod", "pod/mypod"},
		{"pods/mypod", TargetPod, "mypod", "pod/mypod"},
		{"po/mypod", TargetPod, "mypod", "pod/mypod"},
		{"service/db", TargetService, "db", "service/db"},
		{"services/db", TargetService, "db", "service/db"},
		{"svc/db", TargetService, "db", "service/db"},
		{"deployment/web", TargetDeployment, "web", "deployment/web"},
		{"deployments/web", TargetDeployment, "web", "deployment/web"},
		{"deploy/web", TargetDeployment, "web", "deployment/web"},
		{"replicaset/web", TargetReplicaSet, "web", "replicaset/web"},
		{"replicasets/web", TargetReplicaSet, "web", "replicaset/web"},
		{"rs/web", TargetReplicaSet, "web", "replicaset/web"},
		{"statefulset/db", TargetStatefulSet, "db", "statefulset/db"},
		{"statefulsets/db", TargetStatefulSet, "db", "statefulset/db"},
		{"sts/db", TargetStatefulSet, "db", "statefulset/db"},
		{"daemonset/agent", TargetDaemonSet, "agent", "daemonset/agent"},
		{"daemonsets/agent", TargetDaemonSet, "agent", "daemonset/agent"},
		{"ds/agent", TargetDaemonSet, "agent", "daemonset/agent"},
		{"job/migrate", TargetJob, "migrate", "job/migrate"},
		{"jobs/migrate", TargetJob, "migrate", "job/migrate"},
		{"replicationcontroller/old", TargetReplicationController, "old", "replicationcontroller/old"},
		{"replicationcontrollers/old", TargetReplicationController, "old", "replicationcontroller/old"},
		{"rc/old", TargetReplicationController, "old", "replicationcontroller/old"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			target, err := ParseTarget(test.input)
			if err != nil {
				t.Fatalf("ParseTarget(%q) returned error: %s", test.input, err)
			}
			if target.Type != test.wantType || target.Name != test.wantName {
				t.Fatalf("ParseTarget(%q) = %#v, want type %q name %q", test.input, target, test.wantType, test.wantName)
			}
			if target.Canonical() != test.canonical {
				t.Fatalf("Canonical() = %q, want %q", target.Canonical(), test.canonical)
			}
		})
	}
}

func TestParseTargetInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input       string
		wantMessage string
	}{
		{"", "target must not be empty"},
		{" ", "leading or trailing whitespace"},
		{" pod/x", "leading or trailing whitespace"},
		{"/pod", "empty type segment"},
		{"pod/", "empty name segment"},
		{"/", "empty type segment"},
		{"deployment/web/extra", "more than one '/'"},
		{"apps/v1/deployments/web", "more than one '/'"},
		{"unknown/x", "unsupported target type"},
		{"Deployment/web", "unsupported target type"},
		{"deployment/Web", "invalid target"},
		{"deployment/web_1", "invalid target"},
		{"pod/", "empty name segment"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			_, err := ParseTarget(test.input)
			if err == nil {
				t.Fatalf("ParseTarget(%q) expected error", test.input)
			}
			if !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("ParseTarget(%q) error = %q, want it to contain %q", test.input, err, test.wantMessage)
			}
		})
	}
}
