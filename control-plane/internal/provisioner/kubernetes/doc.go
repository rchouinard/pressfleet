// Package kubernetes is the Kubernetes driver (design doc §4.8): the only
// package allowed to import client-go and the other Kubernetes modules
// (§4.2; archtest enforces it).
//
// The driver talks to the API server with client-go's typed clientset, and
// with its dynamic client for server-side apply, which also covers Traefik's
// CRDs. It keeps no informer caches.
//
// Each site is the namespace pf-<site-id>, holding what the site's Compose
// project holds: a single-replica Deployment whose pod runs nginx and php,
// the wp-content PersistentVolumeClaim, wp-config.php in a Secret, a Service
// for nginx, NetworkPolicies, and the RoleBinding that gives the control
// plane its rights there. [Provisioner.RenderSite] renders them from
// templates/k8s into the site's directory under the data directory, where
// operators can inspect them; the site operations apply them with
// server-side apply under the field manager "pressfleet", delete the
// objects that the manifests no longer have ([Client.Prune]), and wait for
// the cluster to converge. Starting and stopping a site scale its
// Deployment between 1 and 0, and deleting it deletes its namespace.
//
// Tasks are pods ([Provisioner.RunTask], [Provisioner.RunRepositoryTask]):
// a site's in its namespace, mounting the data volume through a clone of
// its PersistentVolume there, and a repository's in the platform namespace
// behind a NetworkPolicy of its own. The driver attaches to them for their
// output and deletes them afterwards; [Provisioner.Startup] deletes what a
// restart left.
//
// Routing, exec, sessions, logs, and stats come in later changes (#217,
// #218, #220, #221); until then those methods return [ErrNotImplemented].
package kubernetes
