package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// ErrInvalidManifest is returned for a rendered manifest the driver can't
// apply: malformed YAML, a missing name or namespace, or a kind it doesn't
// know.
var ErrInvalidManifest = errors.New("kubernetes: invalid manifest")

// kind is how the driver reaches the objects of one kind.
type kind struct {
	resource   schema.GroupVersionResource
	namespaced bool
}

// kinds are the object kinds the site manifests may hold (§4.8). A static
// table instead of the API server's discovery: the driver writes only
// these, so it needs no discovery requests or cache, and a manifest of any
// other kind is a template mistake.
var kinds = map[schema.GroupVersionKind]kind{
	{Version: "v1", Kind: "Namespace"}:             {schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, false},
	{Version: "v1", Kind: "Secret"}:                {schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, true},
	{Version: "v1", Kind: "Service"}:               {schema.GroupVersionResource{Version: "v1", Resource: "services"}, true},
	{Version: "v1", Kind: "PersistentVolumeClaim"}: {schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}, true},
	{Group: "apps", Version: "v1", Kind: "Deployment"}: {
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true,
	},
	{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}: {
		schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, true,
	},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}: {
		schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, true,
	},
	{Group: "traefik.io", Version: "v1alpha1", Kind: "IngressRoute"}: {
		schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}, true,
	},
	{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"}: {
		schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares"}, true,
	},
}

// DecodeManifests parses YAML documents into objects, skipping empty
// documents, and checks that each is of a kind the driver knows, with a
// name, and with a namespace if the kind is namespaced.
func DecodeManifests(data []byte) ([]*unstructured.Unstructured, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objs []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return objs, nil
		} else if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
		if len(m) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{Object: m}
		if _, err := kindOf(obj); err != nil {
			return nil, err
		}
		objs = append(objs, obj)
	}
}

// kindOf checks obj and returns how to reach it.
func kindOf(obj *unstructured.Unstructured) (kind, error) {
	gvk := obj.GroupVersionKind()
	k, ok := kinds[gvk]
	switch {
	case !ok:
		return kind{}, fmt.Errorf("%w: unknown kind %s %q", ErrInvalidManifest, obj.GetAPIVersion(), obj.GetKind())
	case obj.GetName() == "":
		return kind{}, fmt.Errorf("%w: a %s without a name", ErrInvalidManifest, gvk.Kind)
	case k.namespaced && obj.GetNamespace() == "":
		return kind{}, fmt.Errorf("%w: %s %s has no namespace", ErrInvalidManifest, gvk.Kind, obj.GetName())
	case !k.namespaced && obj.GetNamespace() != "":
		return kind{}, fmt.Errorf("%w: %s %s is cluster-wide but has a namespace", ErrInvalidManifest, gvk.Kind, obj.GetName())
	}
	return k, nil
}

// Apply applies objects with server-side apply, cluster-wide ones (the
// namespace) first and the others in order. The driver owns these objects,
// so it takes over fields another manager (a kubectl edit) changed. It is
// idempotent: applying the same objects again changes nothing.
func (c *Client) Apply(ctx context.Context, objs []*unstructured.Unstructured) error {
	ordered := slices.Clone(objs)
	slices.SortStableFunc(ordered, func(a, b *unstructured.Unstructured) int {
		ka, kb := kinds[a.GroupVersionKind()], kinds[b.GroupVersionKind()]
		switch {
		case ka.namespaced == kb.namespaced:
			return 0
		case !ka.namespaced:
			return -1
		default:
			return 1
		}
	})
	for _, obj := range ordered {
		k, err := kindOf(obj)
		if err != nil {
			return err
		}
		res := c.dynamic.Resource(k.resource)
		var ri interface {
			Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error)
		} = res
		if k.namespaced {
			ri = res.Namespace(obj.GetNamespace())
		}
		if _, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
			return fmt.Errorf("applying %s %s: %w", obj.GetKind(), objectName(obj), err)
		}
	}
	return nil
}

// ApplyDir applies every manifest file (*.yaml) of dir, as RenderSite
// writes them, in the order of their names.
func (c *Client) ApplyDir(ctx context.Context, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*"+manifestExt))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%w: no manifests in %s", ErrInvalidManifest, dir)
	}
	var objs []*unstructured.Unstructured
	for _, f := range files { // Glob sorts them.
		data, err := os.ReadFile(f) //nolint:gosec // G304: a manifest the driver rendered into the site's directory
		if err != nil {
			return err
		}
		o, err := DecodeManifests(data)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		objs = append(objs, o...)
	}
	return c.Apply(ctx, objs)
}

// objectName is namespace/name, or the name of a cluster-wide object.
func objectName(obj *unstructured.Unstructured) string {
	return strings.TrimPrefix(obj.GetNamespace()+"/"+obj.GetName(), "/")
}
