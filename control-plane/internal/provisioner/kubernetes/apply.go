package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
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

// applyRank orders kinds for Apply: the namespace, then the RoleBinding
// that gives the control plane its rights in it, then the rest.
func applyRank(k kind, gvk schema.GroupVersionKind) int {
	switch {
	case !k.namespaced:
		return 0
	case gvk.Kind == "RoleBinding":
		return 1
	default:
		return 2
	}
}

// Apply applies objects with server-side apply: cluster-wide ones (the
// namespace) first, then RoleBindings, then the others in order. The driver
// owns these objects, so it takes over fields another manager (a kubectl
// edit) changed. It is idempotent: applying the same objects again changes
// nothing.
func (c *Client) Apply(ctx context.Context, objs []*unstructured.Unstructured) error {
	ordered := slices.Clone(objs)
	slices.SortStableFunc(ordered, func(a, b *unstructured.Unstructured) int {
		ga, gb := a.GroupVersionKind(), b.GroupVersionKind()
		return applyRank(kinds[ga], ga) - applyRank(kinds[gb], gb)
	})
	for _, obj := range ordered {
		k, err := kindOf(obj)
		if err != nil {
			return err
		}
		if _, err := c.resource(k, obj.GetNamespace()).Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
			return fmt.Errorf("applying %s %s: %w", obj.GetKind(), objectName(obj), err)
		}
	}
	return nil
}

// resource returns the client of one kind, in namespace if it is
// namespaced.
func (c *Client) resource(k kind, namespace string) dynamic.ResourceInterface {
	res := c.dynamic.Resource(k.resource)
	if k.namespaced {
		return res.Namespace(namespace)
	}
	return res
}

// LoadDir decodes every manifest file (*.yaml) of dir, as RenderSite
// writes them, in the order of their names.
func LoadDir(dir string) ([]*unstructured.Unstructured, error) {
	files, err := readManifests(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: no manifests in %s", ErrInvalidManifest, dir)
	}
	var objs []*unstructured.Unstructured
	for _, name := range slices.Sorted(maps.Keys(files)) {
		o, err := DecodeManifests(files[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
		objs = append(objs, o...)
	}
	return objs, nil
}

// ApplyDir applies every manifest file (*.yaml) of dir (LoadDir).
func (c *Client) ApplyDir(ctx context.Context, dir string) error {
	objs, err := LoadDir(dir)
	if err != nil {
		return err
	}
	return c.Apply(ctx, objs)
}

// Prune deletes the objects in namespace that carry RenderedLabel but
// aren't among keep: what a template rendered before and no longer does.
// Server-side apply only changes the objects it is given, so without this
// an object dropped from the templates would stay behind. Objects without
// the label, such as task pods, are left alone, and so is a kind whose
// resource the cluster doesn't have.
func (c *Client) Prune(ctx context.Context, namespace string, keep []*unstructured.Unstructured) error {
	type key struct {
		gvk  schema.GroupVersionKind
		name string
	}
	wanted := map[key]bool{}
	for _, obj := range keep {
		wanted[key{obj.GroupVersionKind(), obj.GetName()}] = true
	}
	gvks := slices.SortedFunc(maps.Keys(kinds), func(a, b schema.GroupVersionKind) int {
		return strings.Compare(a.String(), b.String())
	})
	for _, gvk := range gvks {
		k := kinds[gvk]
		if !k.namespaced {
			continue
		}
		res := c.resource(k, namespace)
		list, err := res.List(ctx, metav1.ListOptions{LabelSelector: RenderedLabel + "=true"})
		if apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("listing %s in %s: %w", k.resource.Resource, namespace, err)
		}
		for _, item := range list.Items {
			if wanted[key{gvk, item.GetName()}] {
				continue
			}
			if err := res.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting %s %s/%s: %w", gvk.Kind, namespace, item.GetName(), err)
			}
			c.log.InfoContext(ctx, "deleted an object the site's manifests no longer have", "kind", gvk.Kind, "namespace", namespace, "name", item.GetName())
		}
	}
	return nil
}

// objectName is namespace/name, or the name of a cluster-wide object.
func objectName(obj *unstructured.Unstructured) string {
	return strings.TrimPrefix(obj.GetNamespace()+"/"+obj.GetName(), "/")
}
