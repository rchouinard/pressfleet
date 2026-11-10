#!/usr/bin/env bash
# The kind cluster the Kubernetes end-to-end tests run on (docs/testing.md,
# "Kubernetes end-to-end tests"; issue #228).
#
#   scripts/kind/cluster.sh up             create the cluster and its services
#   scripts/kind/cluster.sh push SRC NAME  push local image SRC to the cluster's
#                                          registry as pressfleet/NAME
#   scripts/kind/cluster.sh site-images    push the site images the tests use
#   scripts/kind/cluster.sh down           delete the cluster and the registry
#   scripts/kind/cluster.sh env            print the tests' environment variables
#
# The cluster has what design doc §4.8 asks of one: Calico, a load balancer
# controller (cloud-provider-kind, a container beside the nodes),
# metrics-server, a ReadWriteOnce class (kind's local-path
# "standard") and a ReadWriteMany one ("nfs": csi-driver-nfs and an NFS server
# in the cluster), the kubelet's podPidsLimit, and Kubernetes 1.35 or later.
# Besides, a PostgreSQL for the platforms' state databases, and a registry
# for the images built from the tree, which nodes and control planes both
# reach as $KIND_REGISTRY:5000 (plain HTTP: kubernetes.insecure_registries).
#
# The NFS server is the kernel's, so the host needs the nfsd module, and
# nodes mount with the host's nfs module; up loads both with modprobe when
# they aren't loaded: directly as root, else with passwordless sudo, else
# from a privileged container.
#
# Environment (defaults in brackets):
#   KIND_CLUSTER        cluster name [pfk8s-e2e]
#   KIND_CNI            "calico", or "ptp": the standard ptp plugin and
#                       static routes between the nodes, which ignore
#                       NetworkPolicy, for the test that doctor notices
#                       (TestKubernetesNetworkPolicyIgnored, #225). Not
#                       kind's own kindnet: it enforces NetworkPolicy since
#                       kind 0.24 [calico]
#   KIND_KUBECONFIG     the cluster's kubeconfig [~/.cache/pressfleet-e2e/kubeconfig-$KIND_CLUSTER]
#   KIND_REGISTRY       registry container, its name on the kind network [pfk8s-registry]
#   KIND_REGISTRY_PORT  the registry's port on the host's loopback [5001]
#   KIND_CLOUD_PROVIDER cloud-provider-kind's container [pfk8s-cloud-provider]
#   SITE_PHP            PHP branches of site-images [the two newest]
#   KIND_SITE_IMAGES    "local": push the site images as built (make images)
#                       rather than pulling the published ones [pull]
#
# This is test tooling: the "docker CLI only in provisioner/compose" rule is
# about the Go code, not this script.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
here="$root/scripts/kind"

cluster="${KIND_CLUSTER:-pfk8s-e2e}"
registry="${KIND_REGISTRY:-pfk8s-registry}"
registry_port="${KIND_REGISTRY_PORT:-5001}"
cloud_provider="${KIND_CLOUD_PROVIDER:-pfk8s-cloud-provider}"
# kind.yaml's podSubnet.
pod_cidr="10.244.0.0/16"
cni="${KIND_CNI:-calico}"
case "$cni" in
calico | ptp) ;;
*)
	echo "kind: KIND_CNI must be calico or ptp, not $cni" >&2
	exit 1
	;;
esac
context="kind-$cluster"
# The cluster's kubeconfig, apart from the user's own.
export KUBECONFIG="${KIND_KUBECONFIG:-${XDG_CACHE_HOME:-$HOME/.cache}/pressfleet-e2e/kubeconfig-$cluster}"
mkdir -p "$(dirname "$KUBECONFIG")"

# Pinned versions of what up installs. The node image is kind's default for
# the version in mise.toml.
node_image="kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
calico_version="v3.33.0"
metrics_server_version="v0.9.0"
csi_nfs_version="4.13.5"
registry_image="registry:3@sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c"
cloud_provider_image="registry.k8s.io/cloud-provider-kind/cloud-controller-manager:v0.12.0@sha256:1b97632e5dfec8f83c7e970cf18cedb9e6b707e272011d712cabf6ab59d323c9"

die() {
	echo "kind: $*" >&2
	exit 1
}

log() {
	echo "kind: $*" >&2
}

kc() {
	kubectl --context "$context" "$@"
}

# load_modules makes sure the host kernel has NFS server and client support:
# the NFS server container runs the kernel's nfsd, and nodes mount with the
# kernel's client.
load_modules() {
	local missing=()
	for m in nfs nfsd; do
		grep -qw "^$m" /proc/modules 2>/dev/null || missing+=("$m")
	done
	((${#missing[@]})) || return 0
	log "loading kernel modules: ${missing[*]}"
	if [[ $(id -u) -eq 0 ]]; then
		modprobe -a "${missing[@]}"
	elif sudo -n true 2>/dev/null; then
		sudo modprobe -a "${missing[@]}"
	else
		# No root and no passwordless sudo (a self-hosted runner): Docker
		# access is root-equivalent anyway, so load them from a privileged
		# container. The node image has modprobe, and kind pulls it next.
		docker run --rm --privileged --entrypoint modprobe \
			-v /lib/modules:/lib/modules:ro "$node_image" -a "${missing[@]}"
	fi
}

start_registry() {
	if [[ "$(docker inspect -f '{{.State.Running}}' "$registry" 2>/dev/null)" != true ]]; then
		docker rm -f "$registry" >/dev/null 2>&1 || true
		log "starting registry $registry on 127.0.0.1:$registry_port"
		docker run -d --restart=always --name "$registry" -p "127.0.0.1:$registry_port:5000" \
			--label pressfleet.test=kind "$registry_image" >/dev/null
	fi
}

create_cluster() {
	if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
		# A run that was cancelled or killed leaves its cluster behind,
		# sometimes with its nodes stopped or half removed: reuse it only
		# if its API server answers.
		if kind export kubeconfig --name "$cluster" >/dev/null 2>&1 &&
			kc --request-timeout=20s get --raw /readyz >/dev/null 2>&1; then
			log "cluster $cluster exists; reusing it"
			return
		fi
		log "cluster $cluster exists but doesn't answer; deleting it"
		delete_cluster
	fi
	log "creating cluster $cluster (CNI $cni)"
	kind create cluster --name "$cluster" --image "$node_image" --config "$here/kind.yaml" --wait 0s
}

# install_ptp gives the nodes a network that ignores NetworkPolicy: on each
# node, the standard ptp and host-local plugins (in kind's node image) for
# its pod CIDR, a route to every other node's pod CIDR through that node,
# and masquerading of traffic that leaves the pod network. That is what
# kindnet does, minus its NetworkPolicy controller.
install_ptp() {
	log "configuring the ptp CNI, which ignores NetworkPolicy"
	local nodes node other cidr ip
	nodes="$(kind get nodes --name "$cluster")"
	# The controller manager gives each node its pod CIDR soon after it
	# joins.
	for node in $nodes; do
		for _ in $(seq 60); do
			[[ -z "$(kc get node "$node" -o jsonpath='{.spec.podCIDR}' 2>/dev/null)" ]] || continue 2
			sleep 2
		done
		die "node $node has no pod CIDR"
	done
	for node in $nodes; do
		cidr="$(kc get node "$node" -o jsonpath='{.spec.podCIDR}')"
		printf '%s\n' '{"cniVersion": "0.4.0", "name": "pfk8s-ptp", "plugins": [' \
			'{"type": "ptp", "ipMasq": false, "mtu": 1500, "ipam": {"type": "host-local", "dataDir": "/run/cni-ipam-state",' \
			' "routes": [{"dst": "0.0.0.0/0"}], "ranges": [[{"subnet": "'"$cidr"'"}]]}},' \
			'{"type": "portmap", "capabilities": {"portMappings": true}}]}' |
			docker exec -i "$node" cp /dev/stdin /etc/cni/net.d/10-pfk8s-ptp.conflist
		docker exec "$node" sh -c 'iptables -t nat -C POSTROUTING -s "$1" ! -d "$1" -j MASQUERADE 2>/dev/null ||
			iptables -t nat -A POSTROUTING -s "$1" ! -d "$1" -j MASQUERADE' sh "$pod_cidr"
		for other in $nodes; do
			[[ "$other" != "$node" ]] || continue
			cidr="$(kc get node "$other" -o jsonpath='{.spec.podCIDR}')"
			ip="$(kc get node "$other" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
			docker exec "$node" ip route replace "$cidr" via "$ip"
		done
	done
}

# start_cloud_provider runs cloud-provider-kind, which gives LoadBalancer
# Services an address on the kind network (an Envoy container per Service)
# that the host reaches, as a cloud's load balancer would. It talks to
# Docker, so it gets the socket. It serves every kind cluster on the host,
# and there is only ever this one.
start_cloud_provider() {
	if [[ "$(docker inspect -f '{{.State.Running}}' "$cloud_provider" 2>/dev/null)" == true ]]; then
		return
	fi
	docker rm -f "$cloud_provider" >/dev/null 2>&1 || true
	local socket=/var/run/docker.sock
	if [[ "${DOCKER_HOST:-}" == unix://* ]]; then
		socket="${DOCKER_HOST#unix://}"
	fi
	log "starting cloud-provider-kind as $cloud_provider"
	docker run -d --name "$cloud_provider" --network kind -v "$socket:/var/run/docker.sock" \
		--label pressfleet.test=kind "$cloud_provider_image" --gateway-channel disabled >/dev/null
}

# stop_cloud_provider removes cloud-provider-kind and the load balancers it
# left behind for the cluster.
stop_cloud_provider() {
	docker rm -f "$cloud_provider" >/dev/null 2>&1 || true
	local lbs
	lbs="$(docker ps -aq --filter "label=io.x-k8s.cloud-provider-kind.cluster=$cluster")"
	if [[ -n "$lbs" ]]; then
		# shellcheck disable=SC2086 # one container ID per word
		docker rm -f $lbs >/dev/null
	fi
}

configure_registry() {
	if ! docker network inspect kind -f '{{range .Containers}}{{.Name}} {{end}}' | grep -qw "$registry"; then
		docker network connect kind "$registry"
	fi
	# Nodes pull $registry:5000/... over plain HTTP.
	for node in $(kind get nodes --name "$cluster"); do
		docker exec "$node" mkdir -p "/etc/containerd/certs.d/$registry:5000"
		printf '[host."http://%s:5000"]\n  capabilities = ["pull", "resolve"]\n' "$registry" |
			docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/$registry:5000/hosts.toml"
	done
}

install_calico() {
	log "installing Calico $calico_version"
	# The manifest's pool defaults to 192.168.0.0/16; the cluster's pod
	# network is kind.yaml's podSubnet.
	curl -fsSL "https://raw.githubusercontent.com/projectcalico/calico/$calico_version/manifests/calico.yaml" |
		sed -e 's|# - name: CALICO_IPV4POOL_CIDR|- name: CALICO_IPV4POOL_CIDR|' \
			-e 's|#   value: "192.168.0.0/16"|  value: "'"$pod_cidr"'"|' |
		kc apply --server-side -f - >/dev/null
}

install_metrics_server() {
	log "installing metrics-server $metrics_server_version"
	kc apply -f "https://github.com/kubernetes-sigs/metrics-server/releases/download/$metrics_server_version/components.yaml" >/dev/null
	# kind's kubelets serve self-signed certificates. (Applying the
	# manifest again resets the arguments, so this is always needed.)
	kc -n kube-system patch deployment metrics-server --type=json \
		-p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' >/dev/null
}

install_nfs() {
	log "installing csi-driver-nfs $csi_nfs_version"
	helm --kube-context "$context" upgrade --install csi-driver-nfs \
		"https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts/v$csi_nfs_version/csi-driver-nfs-$csi_nfs_version.tgz" \
		--namespace kube-system --set externalSnapshotter.enabled=false >/dev/null
}

wait_ready() {
	log "waiting for the nodes and system pods"
	kc wait --for=condition=Ready node --all --timeout=5m >/dev/null
	if [[ "$cni" == calico ]]; then
		kc -n kube-system rollout status daemonset/calico-node --timeout=5m >/dev/null
	fi
	kc -n kube-system rollout status deployment/coredns --timeout=5m >/dev/null
	kc -n kube-system rollout status deployment/metrics-server --timeout=5m >/dev/null
	kc -n kube-system rollout status deployment/csi-nfs-controller --timeout=5m >/dev/null
	kc -n kube-system rollout status daemonset/csi-nfs-node --timeout=5m >/dev/null
	kc -n pfk8s-nfs rollout status deployment/nfs-server --timeout=5m >/dev/null
	kc -n pfk8s-postgres rollout status deployment/postgres --timeout=5m >/dev/null
	log "waiting for metrics-server to report nodes"
	for _ in $(seq 120); do
		if kc top nodes >/dev/null 2>&1; then
			return
		fi
		sleep 2
	done
	die "metrics-server reports no node metrics"
}

up() {
	command -v kind >/dev/null || die "kind not found; run 'mise install' and prefix the command with 'mise exec --'"
	command -v kubectl >/dev/null || die "kubectl not found; run 'mise install'"
	command -v helm >/dev/null || die "helm not found; run 'mise install'"
	load_modules
	start_registry
	create_cluster
	configure_registry
	start_cloud_provider
	if [[ "$cni" == calico ]]; then
		install_calico
	else
		install_ptp
	fi
	install_metrics_server
	install_nfs
	kc apply -f "$here/services.yaml" >/dev/null
	wait_ready
	log "cluster $cluster is ready (context $context)"
}

push() {
	local src="$1" name="$2"
	[[ -n "$src" && -n "$name" ]] || die "usage: cluster.sh push <local image> <name:tag>"
	local dst="127.0.0.1:$registry_port/pressfleet/$name"
	log "pushing $src as $registry:5000/pressfleet/$name"
	docker tag "$src" "$dst"
	docker push -q "$dst" >/dev/null
	docker image rm "$dst" >/dev/null
}

# site_images pushes the site images of the latest WordPress on SITE_PHP's
# branches, pulling the published ones that aren't built locally.
site_images() {
	local repo wp php
	repo="$("$root/images/matrix.sh" registry)"
	wp="$("$root/images/matrix.sh" latest-wp)"
	php="${SITE_PHP:-$("$root/images/matrix.sh" php | head -n 2 | xargs)}"
	local images=("site-nginx:wp$wp")
	for v in $php; do
		images+=("site-php:php$v-wp$wp")
	done
	for image in "${images[@]}"; do
		# Tags are rebuilt in place, so a local copy may be stale: pull,
		# unless the images were just built from the tree.
		if [[ "${KIND_SITE_IMAGES:-pull}" != local ]]; then
			log "pulling $repo/$image"
			docker pull -q "$repo/$image" >/dev/null
		fi
		push "$repo/$image" "$image"
	done
}

# delete_cluster deletes the cluster and leaves the registry.
delete_cluster() {
	local status=0
	stop_cloud_provider
	if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
		# A node with a hard NFS mount whose server is gone can't be
		# killed, so the workers, which mount the data volume, go first,
		# while the NFS server on the control plane node still answers
		# (services.yaml).
		for node in $(kind get nodes --name "$cluster"); do
			if [[ "$node" != "$cluster-control-plane" ]]; then
				docker rm -f -v "$node" >/dev/null
			fi
		done
		if ! kind delete cluster --name "$cluster"; then
			log "deleting the cluster failed; retrying once"
			sleep 5
			kind delete cluster --name "$cluster" || status=$?
		fi
	fi
	return "$status"
}

down() {
	local status=0
	delete_cluster || status=$?
	docker rm -f "$registry" >/dev/null 2>&1 || true
	return "$status"
}

# env prints what the tests read to find the cluster (control-plane/internal/e2e/kubernetes.go).
env_vars() {
	echo "PRESSFLEET_K8S_TESTS=1"
	echo "PRESSFLEET_K8S_CONTEXT=$context"
	echo "PRESSFLEET_K8S_KUBECONFIG=$KUBECONFIG"
	echo "PRESSFLEET_K8S_REGISTRY=$registry:5000"
	echo "PRESSFLEET_K8S_CNI=$cni"
}

case "${1:-}" in
up) up ;;
push) push "${2:-}" "${3:-}" ;;
site-images) site_images ;;
down) down ;;
env) env_vars ;;
*) die "usage: cluster.sh up|push|site-images|down|env" ;;
esac
