#!/bin/sh
sed "s/HOSTNAME/$HOSTNAME/" /templates/bird_aggr.toml.template > /conf.d/bird_aggr.toml
sed "s/HOSTNAME/$HOSTNAME/" /templates/bird6_aggr.toml.template > /conf.d/bird6_aggr.toml
exec /confd -confdir=/ -interval=5 -watch --log-level=debug -node ${ETCD_AUTHORITY}
