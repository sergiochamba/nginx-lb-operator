package nginx

import (
    "bytes"
    "context"
    "fmt"
    "path/filepath"
    "strings"
    "text/template"

    "github.com/sergiochamba/nginx-lb-operator/pkg/ipam"
    "golang.org/x/crypto/ssh"
    "golang.org/x/crypto/ssh/knownhosts"
    corev1 "k8s.io/api/core/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "k8s.io/apimachinery/pkg/types"
    "os"
)

var (
    nginxServerIP         string
    nginxUser             string
    nginxSSHPrivateKey    []byte
    nginxKnownHosts       []byte
    nginxConfigDir        = "/etc/nginx/conf.d/"
    nginxNetworkInterface string
)

func Init(client client.Client) error {
    // Load credentials from Kubernetes Secret
    ctx := context.Background()
    secret := &corev1.Secret{}
    secretName := os.Getenv("NGINX_CREDENTIALS_SECRET")
    if secretName == "" {
        secretName = "nginx-server-credentials"
    }
    secretNamespace := os.Getenv("NGINX_CREDENTIALS_NAMESPACE")
    if secretNamespace == "" {
        secretNamespace = "nginx-lb-operator-system"
    }

    err := client.Get(ctx, types.NamespacedName{
        Name:      secretName,
        Namespace: secretNamespace,
    }, secret)
    if err != nil {
        return err
    }

    nginxServerIP = string(secret.Data["NGINX_SERVER_IP"])
    nginxUser = string(secret.Data["NGINX_USER"])
    nginxSSHPrivateKey = secret.Data["NGINX_SSH_PRIVATE_KEY"]
    nginxKnownHosts = secret.Data["NGINX_KNOWN_HOSTS"]
    nginxNetworkInterface = os.Getenv("NGINX_NETWORK_INTERFACE")
    if nginxNetworkInterface == "" {
        nginxNetworkInterface = "eth0"
    }

    return nil
}

func ConfigureService(allocation *ipam.Allocation, ports []int32, endpointIPs []string) error {
    // Assign IP to NGINX interface
    err := AssignIPToNginxServer(allocation.IP)
    if err != nil {
        return err
    }

    // Generate NGINX configuration
    configContent, err := generateNginxConfig(allocation, endpointIPs)
    if err != nil {
        return err
    }

    // Transfer configuration
    err = transferConfigFile(allocation, configContent)
    if err != nil {
        return err
    }

    // Reload NGINX
    err = reloadNginx()
    if err != nil {
        return err
    }

    return nil
}

func RemoveServiceConfiguration(namespace, service string) error {
    // Remove configuration file
    filename := fmt.Sprintf("%s_%s.conf", namespace, service)
    remotePath := filepath.Join(nginxConfigDir, filename)
    cmd := fmt.Sprintf("sudo rm -f %s", remotePath)
    err := executeRemoteCommand(cmd)
    if err != nil {
        return err
    }

    // Reload NGINX
    err = reloadNginx()
    if err != nil {
        return err
    }

    return nil
}

func AssignIPToNginxServer(ip string) error {
    cmd := fmt.Sprintf("sudo ip addr add %s/32 dev %s || true", ip, nginxNetworkInterface)
    return executeRemoteCommand(cmd)
}

func RemoveIPFromNginxServer(ip string) error {
    cmd := fmt.Sprintf("sudo ip addr del %s/32 dev %s || true", ip, nginxNetworkInterface)
    return executeRemoteCommand(cmd)
}

func generateNginxConfig(allocation *ipam.Allocation, endpoints []string) (string, error) {
    tmplPath := filepath.Join("/app", "templates", "nginx.conf.tmpl")
    tmpl, err := template.ParseFiles(tmplPath)
    if err != nil {
        return "", err
    }

    data := struct {
        Namespace string
        Service   string
        IP        string
        Ports     []int32
        Endpoints []string
    }{
        Namespace: allocation.Namespace,
        Service:   allocation.Service,
        IP:        allocation.IP,
        Ports:     allocation.Ports,
        Endpoints: endpoints,
    }

    var buf bytes.Buffer
    err = tmpl.Execute(&buf, data)
    if err != nil {
        return "", err
    }

    return buf.String(), nil
}

func transferConfigFile(allocation *ipam.Allocation, configContent string) error {
    filename := fmt.Sprintf("%s_%s.conf", allocation.Namespace, allocation.Service)
    remotePath := filepath.Join(nginxConfigDir, filename)

    // Establish SSH connection
    client, err := newSSHClient()
    if err != nil {
        return err
    }
    defer client.Close()

    session, err := client.NewSession()
    if err != nil {
        return err
    }
    defer session.Close()

    // Transfer file using 'cat'
    cmd := fmt.Sprintf("sudo tee %s > /dev/null", remotePath)
    session.Stdin = strings.NewReader(configContent)

    err = session.Run(cmd)
    if err != nil {
        return err
    }

    return nil
}

func reloadNginx() error {
    cmd := "sudo nginx -t && sudo nginx -s reload"
    return executeRemoteCommand(cmd)
}

func executeRemoteCommand(cmd string) error {
    client, err := newSSHClient()
    if err != nil {
        return err
    }
    defer client.Close()

    session, err := client.NewSession()
    if err != nil {
        return err
    }
    defer session.Close()

    var stdoutBuf bytes.Buffer
    var stderrBuf bytes.Buffer
    session.Stdout = &stdoutBuf
    session.Stderr = &stderrBuf

    err = session.Run(cmd)
    if err != nil {
        return fmt.Errorf("failed to run command: %s, stderr: %s", err, stderrBuf.String())
    }

    return nil
}

func newSSHClient() (*ssh.Client, error) {
    signer, err := ssh.ParsePrivateKey(nginxSSHPrivateKey)
    if err != nil {
        return nil, err
    }

    hostKeyCallback, err := knownhosts.New(string(nginxKnownHosts))
    if err != nil {
        return nil, err
    }

    sshConfig := &ssh.ClientConfig{
        User: nginxUser,
        Auth: []ssh.AuthMethod{
            ssh.PublicKeys(signer),
        },
        HostKeyCallback: hostKeyCallback,
    }

    client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", nginxServerIP), sshConfig)
    if err != nil {
        return nil, err
    }

    return client, nil
}
