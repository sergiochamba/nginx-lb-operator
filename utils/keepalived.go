package utils

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"text/template"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Embed the templates
//
//go:embed templates/keepalived_primary.conf.tmpl
var keepalivedPrimaryTemplate string

//go:embed templates/keepalived_secondary.conf.tmpl
var keepalivedSecondaryTemplate string

// ConfigureKeepalived generates and updates Keepalived configurations.
// It distributes the allocated IPs into two VIP groups equally.
func ConfigureKeepalived(ctx context.Context, c client.Client, vrid1, vrid2 int) error {
	clusterName := GetClusterName()
	interfaceName := os.Getenv("NGINX_NETWORK_INTERFACE")
	authPass := os.Getenv("KEEPALIVED_AUTH_PASS")
	if authPass == "" {
		authPass = "YourAuthPass" // Default value; replace with secure method
	}

	// Load all allocated IPs to distribute them into groups
	allocatedIPs, err := LoadAllocatedIPs(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to load allocated IPs: %w", err)
	}

	ips := make([]string, 0, len(allocatedIPs))
	for ip := range allocatedIPs {
		ips = append(ips, ip)
	}

	// Sort IPs for consistent distribution
	sort.Strings(ips)

	// Distribute IPs into two groups equally
	group1VIPs, group2VIPs := distributeIPsIntoGroups(ips)

	// Generate primary and secondary configurations
	primaryConfig, err := GenerateKeepalivedConfig(clusterName, interfaceName,
		vrid1, vrid2, authPass, group1VIPs, group2VIPs, true)
	if err != nil {
		return err
	}

	secondaryConfig, err := GenerateKeepalivedConfig(clusterName, interfaceName,
		vrid1, vrid2, authPass, group1VIPs, group2VIPs, false)
	if err != nil {
		return err
	}

	// Define remote paths
	primaryPath := fmt.Sprintf("/etc/keepalived/%s_keepalived.conf", clusterName)
	secondaryPath := fmt.Sprintf("/etc/keepalived/%s_keepalived.conf.secondary", clusterName)

	// Transfer configurations to NGINX server
	if err := CopyFileToNGINXServer(ctx, c, primaryConfig, primaryPath); err != nil {
		return fmt.Errorf("failed to copy primary Keepalived config: %w", err)
	}
	if err := CopyFileToNGINXServer(ctx, c, secondaryConfig, secondaryPath); err != nil {
		return fmt.Errorf("failed to copy secondary Keepalived config: %w", err)
	}

	// Restart Keepalived service
	if err := RestartKeepalived(ctx, c); err != nil {
		return fmt.Errorf("failed to restart Keepalived: %w", err)
	}

	// Wait for VIPs to be updated
	time.Sleep(5 * time.Second)
	return nil
}

// distributeIPsIntoGroups equally distributes IPs into two VIP groups.
// It ensures that the distribution is as balanced as possible.
func distributeIPsIntoGroups(ips []string) ([]string, []string) {
	group1VIPs := []string{}
	group2VIPs := []string{}

	for i, ip := range ips {
		if i%2 == 0 {
			group1VIPs = append(group1VIPs, ip)
		} else {
			group2VIPs = append(group2VIPs, ip)
		}
	}
	return group1VIPs, group2VIPs
}

// GenerateKeepalivedConfig creates the Keepalived configuration content from the template.
func GenerateKeepalivedConfig(clusterName, interfaceName string, vrid1,
	vrid2 int, authPass string, group1VIPs, group2VIPs []string, isPrimary bool) (string, error) {

	var tmplContent string
	if isPrimary {
		tmplContent = keepalivedPrimaryTemplate
	} else {
		tmplContent = keepalivedSecondaryTemplate
	}

	tmpl, err := template.New("keepalived").Parse(tmplContent)
	if err != nil {
		return "", fmt.Errorf("failed to parse Keepalived template: %w", err)
	}

	data := struct {
		ClusterName      string
		Interface        string
		VirtualRouterID1 int
		VirtualRouterID2 int
		AuthPass         string
		Group1VIPs       []string
		Group2VIPs       []string
	}{
		ClusterName:      clusterName,
		Interface:        interfaceName,
		VirtualRouterID1: vrid1,
		VirtualRouterID2: vrid2,
		AuthPass:         authPass,
		Group1VIPs:       group1VIPs,
		Group2VIPs:       group2VIPs,
	}

	var renderedConfig bytes.Buffer
	if err := tmpl.Execute(&renderedConfig, data); err != nil {
		return "", fmt.Errorf("failed to execute Keepalived template: %w", err)
	}

	return renderedConfig.String(), nil
}

// RestartKeepalived restarts the Keepalived service on the NGINX server via SSH.
func RestartKeepalived(ctx context.Context, c client.Client) error {
	command := "sudo systemctl restart keepalived"
	if err := ExecuteSSHCommand(ctx, c, command); err != nil {
		return fmt.Errorf("failed to restart Keepalived service: %w", err)
	}
	return nil
}
