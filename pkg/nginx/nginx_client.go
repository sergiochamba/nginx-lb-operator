package nginx

import (
    "bytes"
    "fmt"
    "io/ioutil"
    "path/filepath"
    "strings"
    "text/template"

    corev1 "k8s.io/api/core/v1"

    "golang.org/x/crypto/ssh"
    "os"

    "github.com/yourusername/nginx-lb-operator/pkg/ipam"
)

var (
    nginxServerIP = os.Getenv("NGINX_SERVER_IP")
    nginxUser     = os.Getenv("NGINX_USER")
    nginxPassword = os.Getenv("NGINX_PASSWORD")
    nginxConfigDir = "/etc/nginx/conf.d/"
)

func ConfigureService(allocation *ipam.Allocation, ports []int32, endpointIPs []string) error {
    // Assign IP to NGINX interface
    err := assignIPToNginxServer(allocation.IP)
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
    cmd := fmt.Sprintf("rm -f %s", remotePath)
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

func assignIPToNginxServer(ip string) error {
    cmd := fmt.Sprintf("sudo ip addr add %s/32 dev eth0 || true", ip)
    return executeRemoteCommand(cmd)
}

func generateNginxConfig(allocation *ipam.Allocation, endpoints []string) (string, error) {
    tmplPath := filepath.Join("pkg", "nginx", "templates", "nginx.conf.tmpl")
    tmpl, err := template.ParseFiles(tmplPath)
    if err != nil {
        return "", err
    }

    data := struct {
        Namespace  string
        Service    string
        IP         string
        Ports      []int32
        Endpoints  []string
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
    cmd := fmt.Sprintf("cat > %s", remotePath)
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
    sshConfig := &ssh.ClientConfig{
        User: nginxUser,
        Auth: []ssh.AuthMethod{
            ssh.Password(nginxPassword),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    }

    client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", nginxServerIP), sshConfig)
    if err != nil {
        return nil, err
    }

    return client, nil
}