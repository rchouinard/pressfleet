// Package kubernetes is the Kubernetes driver (design doc §4.8): the only
// package allowed to import client-go and the other Kubernetes modules
// (§4.2; archtest enforces it).
//
// The driver talks to the API server with client-go's typed clientset, and
// with its dynamic client for server-side apply, which also covers Traefik's
// CRDs. It keeps no informer caches.
//
// [Provisioner.RenderSite] renders a site's manifests from templates/k8s
// into the site's directory under the data directory, where operators can
// inspect them, and [Client.ApplyDir] applies them with server-side apply
// under the field manager "pressfleet". [Client.WaitForRollout] and
// [Client.WaitForNamespaceDeleted] wait for the cluster to converge.
//
// [Provisioner] implements provisioner.Driver. The site lifecycle, routing,
// exec, tasks, sessions, logs, and stats come in later changes (#216 to
// #221); until then those methods return [ErrNotImplemented].
package kubernetes
