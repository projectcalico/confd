module github.com/kelseyhightower/confd

go 1.13

require (
	github.com/BurntSushi/toml v0.3.1
	github.com/golang/protobuf v1.3.1 // indirect
	github.com/google/btree v0.0.0-20180813153112-4030bb1f1f0c // indirect
	github.com/hashicorp/golang-lru v0.5.1 // indirect
	github.com/kelseyhightower/memkv v0.1.1
	github.com/onsi/ginkgo v1.8.0
	github.com/onsi/gomega v1.5.0
	github.com/projectcalico/confd v3.2.0+incompatible // indirect
	github.com/projectcalico/libcalico-go v0.0.0-20191119183141-c072e7a2fae4
	github.com/projectcalico/typha v0.0.0-20191120041510-00ee52d13a55
	github.com/samuel/go-zookeeper v0.0.0-20190801204459-3c104360edc8 // indirect
	github.com/sirupsen/logrus v1.4.2
	github.com/ugorji/go v1.1.7 // indirect
	github.com/xordataexchange/crypt v0.0.2 // indirect
	k8s.io/api v0.0.0-20190718183219-b59d8169aab5
	k8s.io/apimachinery v0.0.0-20190612205821-1799e75a0719
	k8s.io/client-go v12.0.0+incompatible
)

replace github.com/sirupsen/logrus => github.com/projectcalico/logrus v1.0.4-calico
