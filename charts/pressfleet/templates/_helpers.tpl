{{/*
Names, labels, images, and shared fragments of the chart (design doc §4.8).
*/}}

{{/* Labels of every object the chart creates. */}}
{{- define "pressfleet.labels" -}}
app.kubernetes.io/name: pressfleet
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end }}

{{/*
Selector labels of a component's pods: (dict "root" $ "component" "traefik").
Site NetworkPolicies select Traefik's pods by app.kubernetes.io/name and
app.kubernetes.io/component (see the chart's README), so these don't change.
*/}}
{{- define "pressfleet.selector" -}}
app.kubernetes.io/name: pressfleet
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* All labels of a component: (dict "root" $ "component" "traefik"). */}}
{{- define "pressfleet.componentLabels" -}}
{{ include "pressfleet.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Cluster-scoped names carry the platform namespace, so that they can't clash
with another install's, and so the control plane can derive them from
kubernetes.platform_namespace alone.
*/}}
{{- define "pressfleet.siteClusterRole" -}}
pressfleet:{{ .Release.Namespace }}:site
{{- end }}
{{- define "pressfleet.controlPlaneClusterRole" -}}
pressfleet:{{ .Release.Namespace }}:control-plane
{{- end }}
{{- define "pressfleet.traefikClusterRole" -}}
pressfleet:{{ .Release.Namespace }}:traefik
{{- end }}
{{- define "pressfleet.admissionPolicy" -}}
control-plane.{{ .Release.Namespace }}.pressfleet.io
{{- end }}

{{/* The control plane's ServiceAccount, as a user name. */}}
{{- define "pressfleet.controlPlaneUser" -}}
system:serviceaccount:{{ .Release.Namespace }}:control-plane
{{- end }}

{{/*
A pinned image: the override in .Values.images, or else the reference in
files/images.yaml (generated from templates/platform/images.yaml; never edited by
hand). (dict "root" $ "value" "mariadb" "key" "mariadb").
*/}}
{{- define "pressfleet.image" -}}
{{- $override := index .root.Values.images .value -}}
{{- if $override -}}
{{ $override }}
{{- else -}}
{{- $pinned := .root.Files.Get "files/images.yaml" | fromYaml -}}
{{- $ref := index $pinned .key -}}
{{- if not $ref }}{{ fail (printf "images.yaml has no %s image" .key) }}{{ end -}}
{{ $ref }}
{{- end -}}
{{- end }}

{{/*
The control plane image: images.yaml names its repository, and the tag is
the chart's appVersion, so the chart and the control plane come from the
same release.
*/}}
{{- define "pressfleet.controlPlaneImage" -}}
{{- if .Values.images.controlPlane -}}
{{ .Values.images.controlPlane }}
{{- else -}}
{{- $pinned := .Files.Get "files/images.yaml" | fromYaml -}}
{{ index $pinned "control-plane" }}:{{ .Chart.AppVersion }}
{{- end -}}
{{- end }}

{{/* Addresses of the platform services, from any namespace. */}}
{{- define "pressfleet.mariadbAddress" -}}
{{- if .Values.mariadb.enabled -}}
mariadb.{{ .Release.Namespace }}.svc:3306
{{- else -}}
{{ required "mariadb.address is required when mariadb.enabled is false" .Values.mariadb.address }}
{{- end -}}
{{- end }}
{{- define "pressfleet.redisAddress" -}}
redis.{{ .Release.Namespace }}.svc:6379
{{- end }}
{{- define "pressfleet.metricsURL" -}}
http://victoria-metrics.{{ .Release.Namespace }}.svc:8428
{{- end }}

{{/* The storage class of a platform volume: its own, or the sites' class. */}}
{{- define "pressfleet.storageClass" -}}
{{- $class := .own | default .root.Values.kubernetes.storageClasses.sites -}}
{{- if $class -}}
storageClassName: {{ $class | quote }}
{{- end }}
{{- end }}

{{/* imagePullSecrets of the chart's pods. */}}
{{- define "pressfleet.imagePullSecrets" -}}
{{- with .Values.kubernetes.imagePullSecrets -}}
imagePullSecrets:
{{- range . }}
  - name: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
A container securityContext that meets Pod Security "restricted". Add
capabilities with (dict "readOnly" true).
*/}}
{{- define "pressfleet.containerSecurityContext" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: {{ .readOnly }}
  capabilities:
    drop: [ALL]
{{- end }}

{{/* A pod securityContext that meets Pod Security "restricted". */}}
{{- define "pressfleet.podSecurityContext" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: {{ .uid }}
  runAsGroup: {{ .gid }}
  fsGroup: {{ .gid }}
  fsGroupChangePolicy: OnRootMismatch
{{- with .supplementalGroups }}
  supplementalGroups: {{ toJson . }}
{{- end }}
  seccompProfile:
    type: RuntimeDefault
{{- end }}

{{/* Memory requests and limits: the scheduler reserves what the limit allows. */}}
{{- define "pressfleet.memory" -}}
resources:
  requests:
    memory: {{ printf "%dMi" (int .) }}
  limits:
    memory: {{ printf "%dMi" (int .) }}
{{- end }}

{{/* The ACME directory URL Traefik uses, as config.ACMEConfig.Directory. */}}
{{- define "pressfleet.acmeDirectory" -}}
{{- if .Values.acme.caServer -}}
{{ .Values.acme.caServer }}
{{- else if .Values.acme.staging -}}
https://acme-staging-v02.api.letsencrypt.org/directory
{{- else -}}
https://acme-v02.api.letsencrypt.org/directory
{{- end -}}
{{- end }}

{{/* The ACME account settings of Traefik's resolvers. */}}
{{- define "pressfleet.acmeAccount" -}}
{{- with .Values.acme.email -}}
email: {{ . | quote }}
{{ end -}}
caServer: {{ include "pressfleet.acmeDirectory" . | quote }}
storage: /var/lib/traefik/acme.json
{{- end }}

{{/*
NetworkPolicy peers: a platform component's pods, and site pods. Site pods
are those in namespaces labeled as this install's sites: installs may share
a site namespace prefix, so pressfleet.io/platform-namespace tells them
apart (security review K6). Site namespaces created before that label
existed get it from the template rollout after the upgrade; until then the
second peer admits site namespaces without it, which another install's
unlabeled namespaces match too.
*/}}
{{- define "pressfleet.peer" -}}
- podSelector:
    matchLabels:
      {{- include "pressfleet.selector" (dict "component" .) | nindent 6 }}
{{- end }}
{{- define "pressfleet.sitePods" -}}
- namespaceSelector:
    matchLabels:
      app.kubernetes.io/managed-by: pressfleet
      pressfleet.io/platform-namespace: {{ .Release.Namespace | quote }}
    matchExpressions:
      - key: pressfleet.io/site-id
        operator: Exists
- namespaceSelector:
    matchLabels:
      app.kubernetes.io/managed-by: pressfleet
    matchExpressions:
      - key: pressfleet.io/site-id
        operator: Exists
      - key: pressfleet.io/platform-namespace
        operator: DoesNotExist
{{- end }}

{{/*
A CEL condition (admission-policy.yaml): whether the namespace named
variables.target, whose labels are .obj's, is one of this install's site or
doctor probe namespaces: named with the prefix, labeled as a site or probe,
and labeled pressfleet.io/platform-namespace with this install's platform
namespace, because installs may share a prefix (security review K6).

With "legacy" true, a namespace without pressfleet.io/platform-namespace
counts too: one created before the label existed, which the next apply of
its templates labels. Another install's label never counts.
(dict "obj" "object" "sitePrefix" ... "probePrefix" ... "platformNamespace" ... "legacy" false)
*/}}
{{- define "pressfleet.celOwned" -}}
{{- $labels := printf "%s.metadata.labels" .obj -}}
{{- $mine := printf "%s['pressfleet.io/platform-namespace'] == '%s'" $labels .platformNamespace -}}
(has({{ $labels }}) && (
(variables.target.startsWith({{ printf "'%s'" .sitePrefix }}) &&
'app.kubernetes.io/managed-by' in {{ $labels }} && {{ $labels }}['app.kubernetes.io/managed-by'] == 'pressfleet') ||
(variables.target.startsWith({{ printf "'%s'" .probePrefix }}) &&
'pressfleet.io/doctor-probe' in {{ $labels }} && {{ $labels }}['pressfleet.io/doctor-probe'] == 'true')) &&
{{ if .legacy -}}
(!('pressfleet.io/platform-namespace' in {{ $labels }}) || {{ $mine }}))
{{- else -}}
'pressfleet.io/platform-namespace' in {{ $labels }} && {{ $mine }})
{{- end }}
{{- end }}
