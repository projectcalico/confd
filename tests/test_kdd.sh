#!/bin/bash

# Add our bins to the PATH
PATH=$PATH:/calico/bin
TO_TEST=$1

script_dir="$(dirname "$0")"
source "$script_dir/utils.sh"

# This is needed for use in the keys for our templates, both the sed commands
# in utils.sh use them as well as the templates confd uses.
export NODENAME="kube-master"

populate_kdd ${TO_TEST}

get_templates
create_tomls

echo "Running confd against KDD"
confd -kubeconfig=/tests/confd_kubeconfig -onetime -backend=k8s -confdir=/etc/calico/confd -log-level=debug >/dev/null 2>&1 || true
confd -kubeconfig=/tests/confd_kubeconfig -onetime -backend=k8s -confdir=/etc/calico/confd -log-level=debug >/dev/null 2>&1 || true

test_templates ${TO_TEST}
result=$?

return ${result}
