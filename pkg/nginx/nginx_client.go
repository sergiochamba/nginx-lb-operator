package nginx

import (
    "bytes"
    "context"
    "fmt"
	"k8s.io/apimachinery/pkg/api/errors"
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
    k8sClient   client.Client
)

func Init(k8sClientInstance client.Client) error {
    // Store k8sClient globally for VRID operations
    k8sClient = k8sClientInstance
    
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
    return nginxServer.reloadNginx()
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

    return server.writeRemoteFile(remotePath, configContent)
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
    vipList := make([]string, 0, len(allocatedIPs))
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

    // Write primary and secondary configurations to the server
    primaryConfigPath := fmt.Sprintf("/etc/keepalived/%s_keepalived.conf", clusterName)
    log.Log.Info("Writing primary Keepalived configuration", "Path", primaryConfigPath)
    err = server.writeRemoteFile(primaryConfigPath, primaryConfigContent)
    if err != nil {
        log.Log.Error(err, "Failed to write primary Keepalived configuration to server")
        return err
    }
    log.Log.Info("Successfully wrote primary Keepalived configuration")

    secondaryConfigPath := fmt.Sprintf("/etc/keepalived/%s_keepalived.conf.secondary", clusterName)
    log.Log.Info("Writing secondary Keepalived configuration", "Path", secondaryConfigPath)
    err = server.writeRemoteFile(secondaryConfigPath, secondaryConfigContent)
    if err != nil {
        log.Log.Error(err, "Failed to write secondary Keepalived configuration to server")
        return err
    }
    log.Log.Info("Successfully wrote secondary Keepalived configuration")

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
    tmplPath := "/app/templates/"
    if isPrimary {
        tmplPath += "keepalived_primary.conf.tmpl"
    } else {
        tmplPath += "keepalived_secondary.conf.tmpl"
    }

    tmpl, err := template.ParseFiles(tmplPath)
    if err != nil {
        return "", err
    }

    virtualRouterID1, virtualRouterID2, err := getOrAllocateVRIDs("vrid-allocations")
    if err != nil {
        log.Log.Error(err, "Failed to get or allocate VRIDs")
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

func getOrAllocateVRIDs(configMapName string) (int, int, error) {
    ctx := context.Background()
    cm := &corev1.ConfigMap{}
    err := k8sClient.Get(ctx, client.ObjectKey{
        Name:      configMapName,
        Namespace: "nginx-lb-operator-system",
    }, cm)
    
    if err != nil && !errors.IsNotFound(err) {
        return 0, 0, err
    }

    if data, exists := cm.Data["vrid"]; exists && strings.TrimSpace(data) != "" {
        var existingVRID1, existingVRID2 int
        _, err := fmt.Sscanf(data, "%d,%d", &existingVRID1, &existingVRID2)
        if err == nil {
            return existingVRID1, existingVRID2, nil
        }
    }

    // Allocate new VRIDs if none exist in the ConfigMap
    allocatedVRIDs := map[int]bool{}
    virtualRouterID1 := allocateNewVRID(allocatedVRIDs)
    virtualRouterID2 := allocateNewVRID(allocatedVRIDs)

    cm.Data = map[string]string{"vrid": fmt.Sprintf("%d,%d", virtualRouterID1, virtualRouterID2)}
    if errors.IsNotFound(err) {
        cm.Name = configMapName
        cm.Namespace = "nginx-lb-operator-system"
        err = k8sClient.Create(ctx, cm)
    } else {
        err = k8sClient.Update(ctx, cm)
    }

    if err != nil {
        return 0, 0, err
    }

    return virtualRouterID1, virtualRouterID2, nil
}

func allocateNewVRID(allocated map[int]bool) int {
    for i := 1; i <= 255; i++ {
        if !allocated[i] {
            allocated[i] = true
            return i
        }
    }
    return 0
}