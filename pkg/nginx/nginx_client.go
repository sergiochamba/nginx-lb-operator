package nginx

import (
    "bytes"
	"bufio"
    "context"
    "fmt"
    "io/ioutil"
    "path/filepath"
    "sort"
    "strings"
    "text/template"
	"sigs.k8s.io/controller-runtime/pkg/log"
    "github.com/sergiochamba/nginx-lb-operator/pkg/ipam"
    "golang.org/x/crypto/ssh"
    "golang.org/x/crypto/ssh/knownhosts"
    corev1 "k8s.io/api/core/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "os"
)

type NginxServer struct {
    IP               string
    User             string
    SSHPrivateKey    []byte
    KnownHosts       []byte
    NetworkInterface string
}

var (
    nginxServer *NginxServer
    clusterName string
)

func Init(k8sClient client.Client) error {
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

    err := k8sClient.Get(ctx, client.ObjectKey{
        Name:      secretName,
        Namespace: secretNamespace,
    }, secret)
    if err != nil {
        return err
    }

    clusterName = os.Getenv("CLUSTER_NAME")
    if clusterName == "" {
        return fmt.Errorf("CLUSTER_NAME environment variable not set")
    }

    nginxServer = &NginxServer{
        IP:               string(secret.Data["NGINX_SERVER_IP"]),
        User:             string(secret.Data["NGINX_USER"]),
        NetworkInterface: os.Getenv("NGINX_NETWORK_INTERFACE"),
    }
    if nginxServer.NetworkInterface == "" {
        nginxServer.NetworkInterface = "eth0"
    }

    // Read SSH keys from mounted files
    nginxServer.SSHPrivateKey, err = ioutil.ReadFile("/app/ssh/id_rsa")
    if err != nil {
        return err
    }
    nginxServer.KnownHosts, err = ioutil.ReadFile("/app/ssh/known_hosts")
    if err != nil {
        return err
    }

    return nil
}

func ConfigureService(allocation *ipam.Allocation, servicePort int32, endpointIPs []string) error {
    // Generate NGINX configuration
    configContent, err := nginxServer.generateNginxConfig(allocation, servicePort, endpointIPs)
    if err != nil {
        return err
    }

    // Transfer configuration
    err = nginxServer.transferConfigFile(allocation, configContent)
    if err != nil {
        return err
    }

    // Reload NGINX
    err = nginxServer.reloadNginx()
    if err != nil {
        return err
    }

    return nil
}

func RemoveServiceConfiguration(namespace, service string) error {
    filename := fmt.Sprintf("vip-%s-%s-%s.conf", clusterName, namespace, service)
    remotePath := filepath.Join("/etc/nginx/conf.d/", filename)
    cmd := fmt.Sprintf("sudo rm -f %s", remotePath)
    return nginxServer.executeRemoteCommand(cmd)
}

func UpdateKeepalivedConfigs() error {
    log.Log.Info("Updating Keepalived configurations")
    return nginxServer.updateKeepalivedConfigs()
}

// Implement methods on NginxServer

func (server *NginxServer) generateNginxConfig(allocation *ipam.Allocation, servicePort int32, endpoints []string) (string, error) {
    tmplPath := "/app/templates/nginx.conf.tmpl"
    tmpl, err := template.ParseFiles(tmplPath)
    if err != nil {
        return "", err
    }

    data := struct {
        ClusterName string
        Namespace   string
        Service     string
        IP          string
        ServicePort int32
        Endpoints   []string
    }{
        ClusterName: clusterName,
        Namespace:   allocation.Namespace,
        Service:     allocation.Service,
        IP:          allocation.IP,
        ServicePort: servicePort,
        Endpoints:   endpoints,
    }

    var buf bytes.Buffer
    err = tmpl.Execute(&buf, data)
    if err != nil {
        return "", err
    }

    return buf.String(), nil
}

func (server *NginxServer) transferConfigFile(allocation *ipam.Allocation, configContent string) error {
    filename := fmt.Sprintf("vip-%s-%s-%s.conf", clusterName, allocation.Namespace, allocation.Service)
    remotePath := filepath.Join("/etc/nginx/conf.d/", filename)

    client, err := server.newSSHClient()
    if err != nil {
        return err
    }
    defer client.Close()

    session, err := client.NewSession()
    if err != nil {
        return err
    }
    defer session.Close()

    cmd := fmt.Sprintf("sudo tee %s > /dev/null", remotePath)
    session.Stdin = strings.NewReader(configContent)

    err = session.Run(cmd)
    if err != nil {
        return err
    }

    return nil
}

func (server *NginxServer) reloadNginx() error {
    cmd := "sudo nginx -t && sudo nginx -s reload"
    return server.executeRemoteCommand(cmd)
}

func (server *NginxServer) updateKeepalivedConfigs() error {
    // Get allocated IPs
    allocatedIPs, err := ipam.GetAllocatedIPs()
    if err != nil {
        log.Log.Error(err, "Failed to get allocated IPs")
        return err
    }

    log.Log.Info("Allocated IPs retrieved for Keepalived configuration", "AllocatedIPs", allocatedIPs)

    // Convert map to slice and sort
    vipList := []string{}
    for ip := range allocatedIPs {
        vipList = append(vipList, ip)
    }
    sort.Strings(vipList)

    // Balance IPs between two groups
    log.Log.Info("Splitting VIPs into groups for Keepalived")
    group1VIPs, group2VIPs := splitVIPs(vipList)

    // Generate configurations for primary and secondary servers
    log.Log.Info("Generating Keepalived configuration for primary group")
    primaryConfigContent, err := server.generateKeepalivedConfig(group1VIPs, group2VIPs, true)
    if err != nil {
        log.Log.Error(err, "Failed to generate Keepalived configuration for primary group")
        return err
    }
    log.Log.Info("Successfully generated Keepalived configuration for primary group")

    log.Log.Info("Generating Keepalived configuration for secondary group")
    secondaryConfigContent, err := server.generateKeepalivedConfig(group1VIPs, group2VIPs, false)
    if err != nil {
        log.Log.Error(err, "Failed to generate Keepalived configuration for secondary group")
        return err
    }
    log.Log.Info("Successfully generated Keepalived configuration for secondary group")

    // Write primary configuration to the server
    primaryConfigPath := fmt.Sprintf("/etc/keepalived/%s_keepalived.conf", clusterName)
    log.Log.Info("Writing primary Keepalived configuration", "Path", primaryConfigPath)
    err = server.writeRemoteFile(primaryConfigPath, primaryConfigContent)
    if err != nil {
        log.Log.Error(err, "Failed to write primary Keepalived configuration to server")
        return err
    }
    log.Log.Info("Successfully wrote primary Keepalived configuration")

    // Write secondary configuration to a separate file
    secondaryConfigPath := fmt.Sprintf("/etc/keepalived/%s_keepalived.conf.secondary", clusterName)
    log.Log.Info("Writing secondary Keepalived configuration", "Path", secondaryConfigPath)
    err = server.writeRemoteFile(secondaryConfigPath, secondaryConfigContent)
    if err != nil {
        log.Log.Error(err, "Failed to write secondary Keepalived configuration to server")
        return err
    }
    log.Log.Info("Successfully wrote secondary Keepalived configuration")

    // Ensure main keepalived.conf includes the operator's configuration
    //log.Log.Info("Ensuring Keepalived main configuration includes the operator's configuration")
    //err = server.ensureKeepalivedIncludesOperatorConfig(primaryConfigPath)
    //if err != nil {
    //    log.Log.Error(err, "Failed to ensure Keepalived includes operator configuration")
    //    return err
    //}
    //log.Log.Info("Successfully ensured Keepalived configuration includes the operator's config")

    // Reload Keepalived on the primary server
    log.Log.Info("Reloading Keepalived configuration")
    err = server.reloadKeepalived()
    if err != nil {
        log.Log.Error(err, "Failed to reload Keepalived")
        return err
    }
    log.Log.Info("Successfully reloaded Keepalived configuration")

    return nil
}

func splitVIPs(vipList []string) ([]string, []string) {
    half := (len(vipList) + 1) / 2 // Round up to balance if odd number
    group1VIPs := vipList[:half]
    group2VIPs := vipList[half:]
    return group1VIPs, group2VIPs
}

func (server *NginxServer) generateKeepalivedConfig(group1VIPs, group2VIPs []string, isPrimary bool) (string, error) {
    var tmplPath string
    if isPrimary {
        tmplPath = "/app/templates/keepalived_primary.conf.tmpl"
    } else {
        tmplPath = "/app/templates/keepalived_secondary.conf.tmpl"
    }

    tmpl, err := template.ParseFiles(tmplPath)
    if err != nil {
        return "", err
    }

    // Use new function to get unique VRIDs
    virtualRouterID1, err := getUniqueVRID()
    if err != nil {
        log.Log.Error(err, "Failed to allocate VirtualRouterID1")
        return "", err
    }

    virtualRouterID2, err := getUniqueVRID()
    if err != nil {
        log.Log.Error(err, "Failed to allocate VirtualRouterID2")
        return "", err
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
        Interface:        server.NetworkInterface,
        VirtualRouterID1: virtualRouterID1,
        VirtualRouterID2: virtualRouterID2,
        AuthPass:         "StrongPassword", // Should be securely managed
        Group1VIPs:       group1VIPs,
        Group2VIPs:       group2VIPs,
    }

    var buf bytes.Buffer
    err = tmpl.Execute(&buf, data)
    if err != nil {
        return "", err
    }

    return buf.String(), nil
}

func (server *NginxServer) writeRemoteFile(remotePath, content string) error {
    client, err := server.newSSHClient()
    if err != nil {
        return err
    }
    defer client.Close()

    session, err := client.NewSession()
    if err != nil {
        return err
    }
    defer session.Close()

    cmd := fmt.Sprintf("sudo tee %s > /dev/null", remotePath)
    session.Stdin = strings.NewReader(content)

    err = session.Run(cmd)
    if err != nil {
        return err
    }

    return nil
}

func (server *NginxServer) ensureKeepalivedIncludesOperatorConfig(operatorConfigPath string) error {
    mainConfigPath := "/etc/keepalived/keepalived.conf"
    includeStatement := fmt.Sprintf("include %s", operatorConfigPath)

    cmd := fmt.Sprintf(`sudo grep -qF '%s' %s || echo '%s' | sudo tee -a %s`, includeStatement, mainConfigPath, includeStatement, mainConfigPath)
    return server.executeRemoteCommand(cmd)
}

func (server *NginxServer) reloadKeepalived() error {
    cmd := "sudo systemctl restart keepalived"
    return server.executeRemoteCommand(cmd)
}

func (server *NginxServer) executeRemoteCommand(cmd string) error {
    client, err := server.newSSHClient()
    if err != nil {
        return err
    }
    defer client.Close()

    session, err := client.NewSession()
    if err != nil {
        return err
    }
    defer session.Close()

    var stderrBuf bytes.Buffer
    session.Stderr = &stderrBuf

    err = session.Run(cmd)
    if err != nil {
        return fmt.Errorf("failed to run command: %s, stderr: %s", err, stderrBuf.String())
    }

    return nil
}

func (server *NginxServer) newSSHClient() (*ssh.Client, error) {
    signer, err := ssh.ParsePrivateKey(server.SSHPrivateKey)
    if err != nil {
        return nil, err
    }

    hostKeyCallback, err := knownhosts.New("/app/ssh/known_hosts")
    if err != nil {
        return nil, err
    }

    sshConfig := &ssh.ClientConfig{
        User: server.User,
        Auth: []ssh.AuthMethod{
            ssh.PublicKeys(signer),
        },
        HostKeyCallback: hostKeyCallback,
    }

    client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", server.IP), sshConfig)
    if err != nil {
        return nil, err
    }

    return client, nil
}

func hashClusterName(name string) int {
    var hash int
    for _, c := range name {
        hash += int(c)
    }
    return hash % 255
}

func getUniqueVRID() (int, error) {
    vridAllocPath := "/etc/keepalived/VRID_allocations.conf"

    // Open the VRID allocation file remotely using SSH and check if it exists
    client, err := nginxServer.newSSHClient()
    if err != nil {
        log.Log.Error(err, "Failed to create SSH client for VRID allocation")
        return 0, err
    }
    defer client.Close()

    // Use `cat` to check if the VRID file exists, if not, create it
    checkCmd := fmt.Sprintf("sudo touch %s && cat %s", vridAllocPath, vridAllocPath)
    session, err := client.NewSession()
    if err != nil {
        log.Log.Error(err, "Failed to create SSH session")
        return 0, err
    }
    defer session.Close()

    var outputBuf bytes.Buffer
    session.Stdout = &outputBuf
    err = session.Run(checkCmd)
    if err != nil {
        log.Log.Error(err, "Failed to check or create VRID allocations file")
        return 0, err
    }

    // Parse existing VRIDs from the remote file output
    allocatedVRIDs := map[int]bool{}
    scanner := bufio.NewScanner(strings.NewReader(outputBuf.String()))
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }
        var vrid int
        _, err := fmt.Sscanf(line, "vrid %d", &vrid)
        if err != nil {
            log.Log.Error(err, "Failed to parse VRID from file", "Line", line)
            continue
        }
        allocatedVRIDs[vrid] = true
    }
    if err := scanner.Err(); err != nil {
        log.Log.Error(err, "Failed to read VRID allocations")
        return 0, err
    }

    // Find an unused VRID in the range [1, 255]
    var newVRID int
    for i := 1; i <= 255; i++ {
        if !allocatedVRIDs[i] {
            newVRID = i
            break
        }
    }
    if newVRID == 0 {
        return 0, fmt.Errorf("no available VRIDs found")
    }

    // Append the new VRID to the file
    appendCmd := fmt.Sprintf("echo 'vrid %d' | sudo tee -a %s > /dev/null", newVRID, vridAllocPath)
    appendSession, err := client.NewSession()
    if err != nil {
        log.Log.Error(err, "Failed to create SSH session for appending VRID")
        return 0, err
    }
    defer appendSession.Close()

    err = appendSession.Run(appendCmd)
    if err != nil {
        log.Log.Error(err, "Failed to append new VRID to allocations file")
        return 0, err
    }

    log.Log.Info("Successfully allocated new VRID", "VRID", newVRID)
    return newVRID, nil
}