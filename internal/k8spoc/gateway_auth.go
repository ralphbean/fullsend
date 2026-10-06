package k8spoc

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/fullsend-ai/fullsend/internal/sandbox"
)

// refreshGatewayAuth exchanges the mounted service-account secret for a new
// short-lived OpenShell token. Client-credentials tokens have no refresh token,
// so a long-running probe must log in again before each separate CLI operation.
func refreshGatewayAuth() error {
	if os.Getenv("OPENSHELL_OIDC_CLIENT_SECRET") == "" {
		return fmt.Errorf("gateway service-account client secret is required")
	}
	cmd := exec.Command("openshell", "gateway", "login")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard // Never copy authentication output into Job logs.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("refresh gateway service-account login: %w", err)
	}
	return nil
}

func deleteGatewaySandbox(name string) error {
	if err := refreshGatewayAuth(); err != nil {
		return err
	}
	return sandbox.Delete(name)
}
