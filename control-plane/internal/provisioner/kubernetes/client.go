package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager is the field manager of every object the driver applies, so
// that server-side apply tracks which fields PressFleet owns.
const FieldManager = "pressfleet"

// ErrConnect is returned when the driver can't reach or authenticate to the
// API server.
var ErrConnect = errors.New("kubernetes: can't connect to the cluster")

// defaultPollInterval is how often the Wait methods look at the cluster.
const defaultPollInterval = time.Second

// Client is the driver's connection to the API server: the typed clientset
// for built-in objects and the dynamic client for server-side apply.
type Client struct {
	typed   kubernetes.Interface
	dynamic dynamic.Interface
	// poll is the interval of the Wait methods; tests shorten it.
	poll time.Duration
	log  *slog.Logger
}

// newClient wraps clientsets, real or fake.
func newClient(typed kubernetes.Interface, dyn dynamic.Interface, log *slog.Logger) *Client {
	return &Client{typed: typed, dynamic: dyn, poll: defaultPollInterval, log: log}
}

// restConfig returns the connection settings: the kubeconfig file if one is
// given, for development outside the cluster, or else the pod's service
// account.
func restConfig(kubeconfig string) (*rest.Config, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig != "" {
		// Only the file: an operator who sets kubernetes.kubeconfig means
		// that file, not $KUBECONFIG or ~/.kube/config.
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		if kubeconfig == "" {
			return nil, fmt.Errorf("%w: not running in a pod (set kubernetes.kubeconfig to connect from outside the cluster): %w", ErrConnect, err)
		}
		return nil, fmt.Errorf("%w: kubeconfig %s: %w", ErrConnect, kubeconfig, err)
	}
	cfg.UserAgent = "pressfleet"
	// client-go's defaults (5 requests a second, bursts of 10) would slow
	// applying a site's manifests; these stay well within the API server's
	// default priority and fairness limits.
	cfg.QPS, cfg.Burst = 20, 40
	return cfg, nil
}

// connect builds a client from connection settings. It doesn't contact the
// API server; Ping does.
func connect(cfg *rest.Config, log *slog.Logger) (*Client, error) {
	cfg = rest.CopyConfig(cfg)
	// Deprecation warnings from the API server go to the control plane's
	// log rather than client-go's own stderr logger.
	cfg.WarningHandlerWithContext = slogWarnings{log}
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConnect, err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConnect, err)
	}
	return newClient(typed, dyn, log), nil
}

// Ping asks the API server for its version, which proves that the cluster
// is reachable and accepts the driver's credentials.
func (c *Client) Ping(ctx context.Context) (*version.Info, error) {
	rc := c.typed.Discovery().RESTClient()
	if rc == nil {
		// The fake clientset has no REST client.
		return nil, fmt.Errorf("%w: the client has no REST connection", ErrConnect)
	}
	body, err := rc.Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConnect, err)
	}
	var info version.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("%w: reading the server version: %w", ErrConnect, err)
	}
	return &info, nil
}

// slogWarnings logs the API server's warnings (deprecated APIs, Pod
// Security violations it only warns about).
type slogWarnings struct{ log *slog.Logger }

// HandleWarningHeaderWithContext implements rest.WarningHandlerWithContext.
func (w slogWarnings) HandleWarningHeaderWithContext(ctx context.Context, code int, agent, text string) {
	if code != 299 || text == "" {
		return // Only 299 is a warning the API server means for its client.
	}
	w.log.WarnContext(ctx, "the Kubernetes API server warned", "warning", text, "agent", agent)
}
