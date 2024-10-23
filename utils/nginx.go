package utils

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Embed the NGINX template
//
//go:embed templates/nginx.conf.tmpl
var nginxTemplate string

// ConfigureNGINX generates and updates the NGINX configuration for the service.
func ConfigureNGINX(ctx context.Context, c client.Client, service *corev1.Service, ip string) error {
	endpoints, err := GetServiceEndpoints(ctx, c, service)
	if err != nil {
		return fmt.Errorf("failed to get endpoints for service %s/%s: %w", service.Namespace, service.Name, err)
	}

	nginxConfig, err := GenerateNGINXConfig(service, endpoints, ip)
	if err != nil {
		return err
	}

	remotePath := fmt.Sprintf("/etc/nginx/conf.d/vip-%s-%s-%s.conf",
		GetClusterName(), service.Namespace, service.Name)

	if err := CopyFileToNGINXServer(ctx, c, nginxConfig, remotePath); err != nil {
		return fmt.Errorf("failed to copy NGINX config to server: %w", err)
	}

	if err := ReloadNGINX(ctx, c); err != nil {
		return fmt.Errorf("failed to reload NGINX: %w", err)
	}

	return nil
}

// GenerateNGINXConfig creates the NGINX configuration content from the template.
func GenerateNGINXConfig(service *corev1.Service, endpoints []string, ip string) (string, error) {
	tmpl, err := template.New("nginx").Parse(nginxTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse NGINX template: %w", err)
	}

	clusterName := GetClusterName()
	nodePort := service.Spec.Ports[0].NodePort
	servicePort := service.Spec.Ports[0].Port

	upstreamName := fmt.Sprintf("%s_%s_%s", clusterName, service.Namespace, service.Name)

	data := struct {
		UpstreamName string
		Endpoints    []string
		NodePort     int32
		IP           string
		ServicePort  int32
	}{
		UpstreamName: upstreamName,
		Endpoints:    endpoints,
		NodePort:     nodePort,
		IP:           ip,
		ServicePort:  servicePort,
	}

	var renderedConfig bytes.Buffer
	if err := tmpl.Execute(&renderedConfig, data); err != nil {
		return "", fmt.Errorf("failed to execute NGINX template: %w", err)
	}

	return renderedConfig.String(), nil
}

// RemoveNGINXConfig removes the NGINX configuration for the specified service.
func RemoveNGINXConfig(ctx context.Context, c client.Client, service *corev1.Service) error {
	remotePath := fmt.Sprintf("/etc/nginx/conf.d/vip-%s-%s-%s.conf",
		GetClusterName(), service.Namespace, service.Name)

	if err := RemoveFileFromNGINXServer(ctx, c, remotePath); err != nil {
		return fmt.Errorf("failed to remove NGINX config from server: %w", remotePath, err)
	}

	if err := ReloadNGINX(ctx, c); err != nil {
		return fmt.Errorf("failed to reload NGINX after removing config: %w", err)
	}

	return nil
}

// ReloadNGINX reloads the NGINX service on the server via SSH.
func ReloadNGINX(ctx context.Context, c client.Client) error {
	command := "sudo nginx -s reload"
	if err := ExecuteSSHCommand(ctx, c, command); err != nil {
		return fmt.Errorf("failed to reload NGINX: %w", err)
	}
	return nil
}
