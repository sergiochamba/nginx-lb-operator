package utils

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CopyFileToNGINXServer copies a file directly to the NGINX server via SSH and writes it using sudo.
func CopyFileToNGINXServer(ctx context.Context, c client.Client, content, remotePath string) error {
	clientConfig, err := GetSSHClientConfig(ctx, c)
	if err != nil {
		return err
	}

	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", clientConfig.Host), clientConfig.Config)
	if err != nil {
		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Escape the content for use in the command
	escapedContent := strings.ReplaceAll(content, "'", "'\\''")

	// Command to echo the content and write it to the target file using sudo
	command := fmt.Sprintf("echo '%s' | sudo tee %s", escapedContent, remotePath)

	if err := session.Run(command); err != nil {
		return fmt.Errorf("failed to write file to '%s': %w", remotePath, err)
	}

	return nil
}

// RemoveFileFromNGINXServer removes a file directly from the NGINX server via SSH using sudo.
func RemoveFileFromNGINXServer(ctx context.Context, c client.Client, remotePath string) error {
	clientConfig, err := GetSSHClientConfig(ctx, c)
	if err != nil {
		return err
	}

	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", clientConfig.Host), clientConfig.Config)
	if err != nil {
		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Command to remove the file using sudo
	command := fmt.Sprintf("sudo rm %s", remotePath)

	if err := session.Run(command); err != nil {
		return fmt.Errorf("failed to remove file '%s': %w", remotePath, err)
	}

	return nil
}

// ExecuteSSHCommand executes a command on the NGINX server via SSH.
func ExecuteSSHCommand(ctx context.Context, c client.Client, command string) error {
	clientConfig, err := GetSSHClientConfig(ctx, c)
	if err != nil {
		return err
	}

	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", clientConfig.Host), clientConfig.Config)
	if err != nil {
		return fmt.Errorf("failed to establish SSH connection: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Run the command
	if err := session.Run(command); err != nil {
		return fmt.Errorf("failed to execute command '%s': %w", command, err)
	}

	return nil
}

// GetSSHClientConfig retrieves SSH client configuration from the Kubernetes Secret.
func GetSSHClientConfig(ctx context.Context, c client.Client) (*SSHClientConfig, error) {
	secretName := os.Getenv("NGINX_CREDENTIALS_SECRET")
	namespace := os.Getenv("NGINX_CREDENTIALS_NAMESPACE")

	if secretName == "" || namespace == "" {
		return nil, fmt.Errorf("NGINX_CREDENTIALS_SECRET and NGINX_CREDENTIALS_NAMESPACE must be set")
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: namespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("failed to get SSH credentials secret: %w", err)
	}

	nginxServerIP := string(secret.Data["NGINX_SERVER_IP"])
	nginxUser := string(secret.Data["NGINX_USER"])
	privateKey := secret.Data["NGINX_SSH_PRIVATE_KEY"]
	knownHostsData := secret.Data["NGINX_KNOWN_HOSTS"]

	if nginxServerIP == "" || nginxUser == "" || len(privateKey) == 0 || len(knownHostsData) == 0 {
		return nil, fmt.Errorf("incomplete SSH credentials in secret")
	}

	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	// Write known_hosts to a temp file
	knownHostsFile, err := ioutil.TempFile("", "known_hosts")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file for known_hosts: %w", err)
	}
	defer os.Remove(knownHostsFile.Name())

	if _, err := knownHostsFile.Write(knownHostsData); err != nil {
		return nil, fmt.Errorf("failed to write known_hosts data: %w", err)
	}
	knownHostsFile.Close()

	hostKeyCallback, err := knownhosts.New(knownHostsFile.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to create host key callback: %w", err)
	}

	config := &ssh.ClientConfig{
		User: nginxUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: hostKeyCallback,
	}

	return &SSHClientConfig{
		Host:   nginxServerIP,
		Config: config,
	}, nil
}

// SSHClientConfig holds the SSH client configuration details.
type SSHClientConfig struct {
	Host   string
	Config *ssh.ClientConfig
}
