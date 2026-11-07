{{/*
The control plane's pod spec, shared by its Deployment and by the state
recovery Job (state-recover.yaml), which runs `pressfleet state recover` with
the same settings, Secrets, data volume, and ServiceAccount (its repository
tasks are pods the driver starts). (dict "root" $ "args" (list "serve")
"serve" true): serve adds the ports and probes of the running control plane.
*/}}
{{- define "pressfleet.controlPlanePodSpec" -}}
{{- $root := .root }}
{{- $dataDir := "/var/lib/pressfleet" -}}
serviceAccountName: control-plane
# The driver talks to the API server as this ServiceAccount (RBAC in
# rbac.yaml).
automountServiceAccountToken: true
{{- with include "pressfleet.imagePullSecrets" $root }}{{ . | nindent 0 }}{{ end }}
{{- /*
  Non-root, unlike on Compose, where the control plane needs the
  Docker socket. The repository group lets it share local backup
  repositories with tasks (provisioner.RepositoryGID).
*/}}
{{- include "pressfleet.podSecurityContext" (dict "uid" $root.Values.controlPlane.uid "gid" $root.Values.controlPlane.gid "supplementalGroups" (list 60000)) | nindent 0 }}
{{- if not .serve }}
restartPolicy: Never
{{- end }}
containers:
  - name: control-plane
    image: {{ include "pressfleet.controlPlaneImage" $root | quote }}
    args: {{ toJson .args }}
    env:
      # The Go process's share of the memory limit, as on Compose (§4.1).
      - name: GOMEMLIMIT
        value: "96MiB"
      - name: PRESSFLEET_DRIVER
        value: kubernetes
      - name: PRESSFLEET_DATA_DIR
        value: {{ $dataDir | quote }}
      - name: PRESSFLEET_LISTEN
        value: ":8080"
      - name: PRESSFLEET_MASTER_KEY_FILE
        value: /run/secrets/master_key
      {{- if $root.Values.config.secretName }}
      # The operator's config file, for the settings not passed here.
      # These variables take precedence over it.
      - name: PRESSFLEET_CONFIG
        value: /run/secrets/pressfleet_config
      {{- end }}
      - name: PRESSFLEET_DATABASE_DRIVER
        value: postgres
      - name: PRESSFLEET_DATABASE_POSTGRES_DSN
        valueFrom:
          secretKeyRef:
            name: {{ $root.Values.database.postgresDSN.secretName | quote }}
            key: {{ $root.Values.database.postgresDSN.key | quote }}
      # Tasks run in the control plane's own image (§5.3).
      - name: PRESSFLEET_TASK_IMAGE
        value: {{ include "pressfleet.controlPlaneImage" $root | quote }}
      # Full names, so that sites in their own namespaces reach them too.
      - name: PRESSFLEET_MARIADB_ADDRESS
        value: {{ include "pressfleet.mariadbAddress" $root | quote }}
      - name: PRESSFLEET_REDIS_ADDRESS
        value: {{ include "pressfleet.redisAddress" $root | quote }}
      - name: PRESSFLEET_METRICS_URL
        value: {{ include "pressfleet.metricsURL" $root | quote }}
      - name: PRESSFLEET_METRICS_RETENTION_DAYS
        value: {{ $root.Values.metrics.retentionDays | quote }}
      - name: PRESSFLEET_SFTP_PORT
        value: {{ $root.Values.sftp.port | quote }}
      {{- with $root.Values.dashboardDomain }}
      - name: PRESSFLEET_DASHBOARD_DOMAIN
        value: {{ . | quote }}
      {{- end }}
      {{- with $root.Values.previewBaseDomain }}
      - name: PRESSFLEET_PREVIEW_BASE_DOMAIN
        value: {{ . | quote }}
      {{- end }}
      {{- with $root.Values.publicIP }}
      - name: PRESSFLEET_PUBLIC_IP
        value: {{ . | quote }}
      {{- end }}
      - name: PRESSFLEET_KUBERNETES_PLATFORM_NAMESPACE
        value: {{ $root.Release.Namespace | quote }}
      - name: PRESSFLEET_KUBERNETES_SITE_NAMESPACE_PREFIX
        value: {{ $root.Values.kubernetes.siteNamespacePrefix | quote }}
      - name: PRESSFLEET_KUBERNETES_STORAGE_CLASSES_SITES
        value: {{ $root.Values.kubernetes.storageClasses.sites | quote }}
      - name: PRESSFLEET_KUBERNETES_STORAGE_CLASSES_DATA
        value: {{ $root.Values.kubernetes.storageClasses.data | quote }}
      - name: PRESSFLEET_KUBERNETES_TRAEFIK_SERVICE
        value: traefik
      # Task pods mount it too (#219).
      - name: PRESSFLEET_KUBERNETES_DATA_CLAIM
        value: {{ $root.Values.controlPlane.data.existingClaim | default "control-plane-data" | quote }}
      - name: PRESSFLEET_KUBERNETES_POD_CIDRS
        value: {{ join "," $root.Values.kubernetes.podCIDRs | quote }}
      - name: PRESSFLEET_KUBERNETES_IMAGE_PULL_SECRETS
        value: {{ join "," $root.Values.kubernetes.imagePullSecrets | quote }}
    {{- if .serve }}
    ports:
      - name: http
        containerPort: 8080
      # The SFTP gateway (§5.8); Traefik forwards sftp.port to it.
      - name: sftp
        containerPort: 2222
    {{- end }}
    volumeMounts:
      - name: data
        mountPath: {{ $dataDir | quote }}
      # The master key, the platform credentials, and the config file,
      # at the paths the config defaults name, as on Compose.
      - name: secrets
        mountPath: /run/secrets
        readOnly: true
      - name: tmp
        mountPath: /tmp
    {{- include "pressfleet.containerSecurityContext" (dict "readOnly" true) | nindent 4 }}
    {{- include "pressfleet.memory" $root.Values.controlPlane.memoryMB | nindent 4 }}
    {{- if .serve }}
    # Migrations run at startup, before the API answers.
    startupProbe:
      exec:
        command: ["pressfleet", "healthcheck"]
      periodSeconds: 2
      timeoutSeconds: 5
      failureThreshold: 300
    livenessProbe:
      exec:
        command: ["pressfleet", "healthcheck"]
      periodSeconds: 30
      timeoutSeconds: 5
      failureThreshold: 3
    readinessProbe:
      exec:
        command: ["pressfleet", "healthcheck"]
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
    {{- end }}
volumes:
  - name: data
    persistentVolumeClaim:
      claimName: {{ $root.Values.controlPlane.data.existingClaim | default "control-plane-data" | quote }}
  - name: secrets
    projected:
      # Readable by the control plane's group (fsGroup) only.
      defaultMode: 0440
      sources:
        - secret:
            name: {{ $root.Values.masterKey.secretName | quote }}
            items:
              - key: {{ $root.Values.masterKey.key | quote }}
                path: master_key
        - secret:
            name: {{ $root.Values.platformSecrets.secretName | quote }}
            items:
              - key: {{ $root.Values.platformSecrets.mariadbRootPasswordKey | quote }}
                path: mariadb_root_password
              - key: {{ $root.Values.platformSecrets.redisAdminPasswordKey | quote }}
                path: redis_admin_password
              - key: {{ $root.Values.platformSecrets.sshHostKeyKey | quote }}
                path: ssh_host_key
        {{- with $root.Values.config.secretName }}
        - secret:
            name: {{ . | quote }}
            items:
              - key: {{ $root.Values.config.key | quote }}
                path: pressfleet_config
        {{- end }}
  - name: tmp
    emptyDir: {}
{{- end }}
