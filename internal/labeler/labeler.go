package labeler

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	AnnotationIPv4 = "bulb.toturi.tech/public-ipv4"
	AnnotationIPv6 = "bulb.toturi.tech/public-ipv6"
)

// detectPublicIPs finds the public IPv4 and IPv6 addresses on the
// default-route interface by reading /proc/net/route.
var detectPublicIPs = defaultDetectPublicIPs

func defaultDetectPublicIPs() (ipv4, ipv6 string, err error) {
	iface, err := defaultRouteInterface()
	if err != nil {
		return "", "", fmt.Errorf("detect default route interface: %w", err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", "", fmt.Errorf("list addresses on %s: %w", iface.Name, err)
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		if ip.To4() != nil && !isPrivateV4(ip) && ipv4 == "" {
			ipv4 = ip.String()
		}
		if ip.To4() == nil && ipv6 == "" {
			ipv6 = ip.String()
		}
	}
	return ipv4, ipv6, nil
}

func isPrivateV4(ip net.IP) bool {
	private := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}
	for _, cidr := range private {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func defaultRouteInterface() (*net.Interface, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, fmt.Errorf("open /proc/net/route: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if fields[1] == "00000000" && fields[2] != "00000000" {
			iface, err := net.InterfaceByName(fields[0])
			if err != nil {
				return nil, fmt.Errorf("lookup interface %s: %w", fields[0], err)
			}
			return iface, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan /proc/net/route: %w", err)
	}
	return nil, fmt.Errorf("no default route found in /proc/net/route")
}

// Run is the `bulb node-ip-labeler` subcommand entrypoint.
func Run(args []string) error {
	fs := flag.NewFlagSet("node-ip-labeler", flag.ContinueOnError)
	nodeName := fs.String("node-name", "", "name of this Node (or set NODE_NAME env)")
	interval := fs.Duration("interval", 60*time.Second, "how often to re-check IPs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *nodeName == "" {
		*nodeName = lookupEnv("NODE_NAME", "")
	}
	if *nodeName == "" {
		return fmt.Errorf("--node-name or NODE_NAME env is required")
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("get in-cluster config: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	return runLoop(ctx, c, *nodeName, *interval)
}

func runLoop(ctx context.Context, c client.Client, nodeName string, interval time.Duration) error {
	logger := log.FromContext(ctx).WithValues("node", nodeName)

	if err := reconcileOnce(ctx, c, nodeName); err != nil {
		logger.Error(err, "initial annotation failed")
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return nil
		case <-ticker.C:
			if err := reconcileOnce(ctx, c, nodeName); err != nil {
				logger.Error(err, "annotation reconcile failed")
			}
		}
	}
}

// ReconcileOnce discovers this node's public IPs and annotates the Node
// object. Exported for testing.
func ReconcileOnce(ctx context.Context, c client.Client, nodeName string, detectFn func() (string, string, error)) error {
	ipv4, ipv6, err := detectFn()
	if err != nil {
		return fmt.Errorf("detect public IPs: %w", err)
	}
	return annotateNode(ctx, c, nodeName, ipv4, ipv6)
}

func reconcileOnce(ctx context.Context, c client.Client, nodeName string) error {
	return ReconcileOnce(ctx, c, nodeName, detectPublicIPs)
}

func annotateNode(ctx context.Context, c client.Client, nodeName, ipv4, ipv6 string) error {
	logger := log.FromContext(ctx).WithValues("node", nodeName)
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	changed := false
	ann := node.Annotations
	if ann == nil {
		ann = make(map[string]string)
	}
	if ipv4 != "" && ann[AnnotationIPv4] != ipv4 {
		ann[AnnotationIPv4] = ipv4
		changed = true
	}
	if ipv6 != "" && ann[AnnotationIPv6] != ipv6 {
		ann[AnnotationIPv6] = ipv6
		changed = true
	}
	if !changed {
		return nil
	}

	patched := node.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = make(map[string]string)
	}
	for k, v := range ann {
		patched.Annotations[k] = v
	}
	if err := c.Update(ctx, patched); err != nil {
		return fmt.Errorf("annotate node: %w", err)
	}
	logger.Info("annotated node with public IPs", "ipv4", ipv4, "ipv6", ipv6)
	return nil
}

func lookupEnv(key, fallback string) string {
	if v, ok := syscall.Getenv(key); ok {
		return v
	}
	return fallback
}
