./clean
./build
docker rm -f calico-confd
docker build -t calico/confd:latest .
# TODO - Use the correct etcd IP here
docker run -d -v /var/run/docker.sock:/docker.sock --name calico-confd -e HOSTNAME=$HOSTNAME -e IP=10.0.2.15 -e IP6=10.0.2.15 -e ETCD_AUTHORITY=10.0.2.15:2379 calico/confd:latest
