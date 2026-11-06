#!/bin/sh
# Container health check: asks the internal status server's /healthz.
#
# The same command as the php image's pressfleet-healthcheck, so an exec probe
# works the same way in both containers. Under Kubernetes it is the right
# probe for nginx: the status port only answers NGINX_STATUS_ALLOW (loopback by
# default), and an httpGet probe comes from the node's address.
set -eu

exec wget -q -O /dev/null http://127.0.0.1:8081/healthz
