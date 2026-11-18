package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain normalizes TF_ACC_TERRAFORM_PATH to an absolute path. CI selects the
// CLI with a bare command name (terraform or tofu), while terraform-exec
// requires an absolute path.
func TestMain(m *testing.M) {
	path := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if path != "" && !filepath.IsAbs(path) {
		if resolved, err := exec.LookPath(path); err == nil {
			path = resolved
			_ = os.Setenv("TF_ACC_TERRAFORM_PATH", resolved)
		}
	}
	// OpenTofu resolves unqualified provider addresses against
	// registry.opentofu.org, while Terraform uses registry.terraform.io. The
	// testing framework builds its reattach addresses from this variable.
	if strings.Contains(strings.ToLower(filepath.Base(path)), "tofu") {
		_ = os.Setenv("TF_ACC_PROVIDER_HOST", "registry.opentofu.org")
	}
	os.Exit(m.Run())
}
