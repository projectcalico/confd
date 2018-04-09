###############################################################################
# The build architecture is select by setting the ARCH variable.
# For example: When building on ppc64le you could use ARCH=ppc64le make <....>.
# When ARCH is undefined it defaults to amd64.
HOSTARCH ?= $(shell uname -m)
GOBUILD_ARCH =
GO_BUILD_VER?=latest
ARCH?=amd64
ALL_ARCH = amd64 arm64 ppc64le s390x
ARCHTAG =

MANIFEST_TOOL_DIR := $(shell mktemp -d)
export PATH := $(MANIFEST_TOOL_DIR):$(PATH)

MANIFEST_TOOL_VERSION := v0.7.0

space :=
space +=
comma := ,
prefix_linux = $(addprefix linux/,$(strip $1))
join_platforms = $(subst $(space),$(comma),$(call prefix_linux,$(strip $1)))

ifeq ($(HOSTARCH),aarch64)
	override GOBUILD_ARCH=-arm64
endif
ifeq ($(HOSTARCH),ppc64le)
	override GOBUILD_ARCH=-ppc64le
endif
ifeq ($(HOSTARCH),s390x)
	override GOBUILD_ARCH=-s390x
endif

ifneq ($(ARCH),amd64)
	override ARCHTAG=-$(ARCH)
endif

# Select which release branch to test.
RELEASE_BRANCH?=master

# Disable make's implicit rules, which are not useful for golang, and slow down the build
# considerably.
.SUFFIXES:

all: clean test

GO_BUILD_CONTAINER?=calico/go-build$(GOBUILD_ARCH):$(GO_BUILD_VER)

CALICOCTL_VER=master
CALICOCTL_CONTAINER_NAME=calico/ctl$(ARCHTAG):$(CALICOCTL_VER)
K8S_VERSION=v1.8.1
ETCD_VER=v3.2.5
BIRD_VER=v0.3.1
LOCAL_IP_ENV?=$(shell ip route get 8.8.8.8 | head -1 | awk '{print $$7}')

CONFD_VERSION?=$(shell git describe --tags --dirty --always)
LDFLAGS=-ldflags "-X main.VERSION=$(CONFD_VERSION)"

# Ensure that the bin directory is always created
MAKE_SURE_BIN_EXIST := $(shell mkdir -p bin)

# All go files.
GO_FILES:=$(shell find . -type f -name '*.go')

# Figure out the users UID.  This is needed to run docker containers
# as the current user and ensure that files built inside containers are
# owned by the current user.
MY_UID:=$(shell id -u)

DOCKER_GO_BUILD := mkdir -p .go-pkg-cache && \
                   docker run --rm \
                              --net=host \
                              $(EXTRA_DOCKER_ARGS) \
                              -e LOCAL_USER_ID=$(MY_UID) \
                              -v ${CURDIR}:/go/src/github.com/kelseyhightower/confd:rw \
                              -v ${CURDIR}/.go-pkg-cache:/go/pkg:rw \
                              -w /go/src/github.com/kelseyhightower/confd \
                              $(GO_BUILD_CONTAINER)

# Update the vendored dependencies with the latest upstream versions matching
# our glide.yaml.  If there are any changes, this updates glide.lock
# as a side effect.  Unless you're adding/updating a dependency, you probably
# want to use the vendor target to install the versions from glide.lock.
.PHONY: update-vendor
update-vendor:
	mkdir -p $$HOME/.glide
	$(DOCKER_GO_BUILD) glide up --strip-vendor
	touch vendor/.up-to-date

# vendor is a shortcut for force rebuilding the go vendor directory.
.PHONY: vendor
vendor vendor/.up-to-date: glide.lock
	mkdir -p $$HOME/.glide
	$(DOCKER_GO_BUILD) glide install --strip-vendor
	touch vendor/.up-to-date

.PHONY: all-container
all-container: $(addprefix sub-container-,$(ALL_ARCH))
sub-container-%:
	$(MAKE) bin/confd ARCH=$*
	$(MAKE) build-confd-container ARCH=$*

.PHONY: build-confd-container
build-confd-container:
	docker build -t calico/confd-$(ARCH) -f Dockerfile.$(ARCH) .

.PHONY: container
container: sub-container-$(ARCH)

.PHONY: build
build: bin/confd

.PHONY: image
image: container

.PHONY: bin/confd
bin/confd: $(GO_FILES) vendor/.up-to-date
	@echo Building confd...
	$(DOCKER_GO_BUILD) \
	    sh -c 'GOARCH=$(ARCH) go build -v -i -o $@$(ARCHTAG) $(LDFLAGS) "github.com/kelseyhightower/confd" && \
		( ldd bin/confd$(ARCHTAG) 2>&1 | grep -q -e "Not a valid dynamic program" \
			-e "not a dynamic executable" || \
	             ( echo "Error: bin/confd was not statically linked"; false ) )'

.PHONY: test
## Run all tests
test: test-kdd test-etcd

.PHONY: test-kdd
## Run template tests against KDD
test-kdd: bin/confd bin/kubectl bin/bird bin/bird6 bin/allocate-ipip-addr bin/calicoctl run-k8s-apiserver
	docker run --rm --net=host \
		-v $(CURDIR)/tests/:/tests/ \
		-v $(CURDIR)/bin:/calico/bin/ \
		-e RELEASE_BRANCH=$(RELEASE_BRANCH) \
		-e LOCAL_USER_ID=0 \
		$(GO_BUILD_CONTAINER) /tests/test_suite_kdd.sh

.PHONY: test-etcd
## Run template tests against etcd
test-etcd: bin/confd bin/etcdctl bin/bird bin/bird6 bin/allocate-ipip-addr bin/calicoctl run-etcd
	docker run --rm --net=host \
		-v $(CURDIR)/tests/:/tests/ \
		-v $(CURDIR)/bin:/calico/bin/ \
		-e RELEASE_BRANCH=$(RELEASE_BRANCH) \
		-e LOCAL_USER_ID=0 \
		$(GO_BUILD_CONTAINER) /tests/test_suite_etcd.sh

.PHONY: test-etcd
## Run template tests against etcd
run-build: bin/confd bin/etcdctl bin/bird bin/bird6 bin/calicoctl run-etcd
	docker run --rm --net=host \
		-v $(CURDIR)/tests/:/tests/ \
		-v $(CURDIR)/bin:/calico/bin/ \
		-e LOCAL_USER_ID=0 \
		-tid \
		--name calico-build \
		$(GO_BUILD_CONTAINER) sh

## Etcd is used by the kubernetes
run-etcd: stop-etcd
	docker run --detach \
	--net=host \
	--name calico-etcd quay.io/coreos/etcd:$(ETCD_VER)$(ARCHTAG) \
	etcd \
	--advertise-client-urls "http://$(LOCAL_IP_ENV):2379,http://127.0.0.1:2379,http://$(LOCAL_IP_ENV):4001,http://127.0.0.1:4001" \
	--listen-client-urls "http://0.0.0.0:2379,http://0.0.0.0:4001"

## Stops calico-etcd containers
stop-etcd:
	@-docker rm -f calico-etcd

## Kubernetes apiserver used for tests
run-k8s-apiserver: stop-k8s-apiserver run-etcd
	docker run --detach --net=host \
	  --name calico-k8s-apiserver \
	gcr.io/google_containers/hyperkube-$(ARCH):$(K8S_VERSION) \
		  /hyperkube apiserver --etcd-servers=http://$(LOCAL_IP_ENV):2379 \
		  --service-cluster-ip-range=10.101.0.0/16 

## Stop Kubernetes apiserver
stop-k8s-apiserver:
	@-docker rm -f calico-k8s-apiserver

bin/kubectl:
	curl -sSf -L --retry 5 https://storage.googleapis.com/kubernetes-release/release/$(K8S_VERSION)/bin/linux/$(ARCH)/kubectl -o $@
	chmod +x $@

bin/bird:
	curl -sSf -L --retry 5 https://github.com/projectcalico/bird/releases/download/$(BIRD_VER)/bird -o $@
	chmod +x $@

bin/bird6:
	curl -sSf -L --retry 5 https://github.com/projectcalico/bird/releases/download/$(BIRD_VER)/bird6 -o $@
	chmod +x $@

bin/allocate-ipip-addr:
	cp fakebinary $@
	chmod +x $@

bin/etcdctl:
	curl -sSf -L --retry 5  https://github.com/coreos/etcd/releases/download/$(ETCD_VER)/etcd-$(ETCD_VER)-linux-$(ARCH).tar.gz | tar -xz -C bin --strip-components=1 etcd-$(ETCD_VER)-linux-$(ARCH)/etcdctl 

bin/calicoctl:
	-docker rm -f calico-ctl
	# Latest calicoctl binaries are stored in automated builds of calico/ctl.
	# To get them, we create (but don't start) a container from that image.
	docker pull $(CALICOCTL_CONTAINER_NAME)
	docker create --name calico-ctl $(CALICOCTL_CONTAINER_NAME)
	# Then we copy the files out of the container.  Since docker preserves
	# mtimes on its copy, check the file really did appear, then touch it
	# to make sure that downstream targets get rebuilt.
	docker cp calico-ctl:/calicoctl $@ && \
	  test -e $@ && \
	  touch $@
	-docker rm -f calico-ctl

all-tag-images: $(addprefix tag-images-,$(ALL_ARCH))
tag-images-%:
	docker tag calico/confd-$* calico/confd-$*:$(VERSION)
	docker tag calico/confd-$* quay.io/calico/confd-$*:$(VERSION)
	docker tag calico/confd-$* quay.io/calico/confd-$*:latest
	@if [ "$*" = amd64 ]; then \
		docker tag calico/confd-$* quay.io/calico/confd:$(VERSION);\
		docker tag calico/confd-$* quay.io/calico/confd:latest;\
	fi

push-all: $(addprefix .push-all-,$(ALL_ARCH))
.push-all-%:
	docker push calico/confd-$*:$(VERSION)
	docker push quay.io/calico/confd-$*:$(VERSION)
	@if [ "$*" = amd64 ]; then \
		docker push quay.io/calico/confd:$(VERSION);\
	fi

push-all-latest: $(addprefix .push-all-latest-,$(ALL_ARCH))
.push-all-latest-%:
	docker push calico/confd-$*:latest
	@if [ "$*" = amd64 ]; then \
		docker push quay.io/calico/confd:latest;\
	endif

manifest-tool:
	curl -sSL https://github.com/estesp/manifest-tool/releases/download/$(MANIFEST_TOOL_VERSION)/manifest-tool-linux-$(ARCH) > $(MANIFEST_TOOL_DIR)/manifest-tool
	chmod +x $(MANIFEST_TOOL_DIR)/manifest-tool

push-manifest: manifest-tool
ifndef VERSION
	$(error VERSION is undefined - run using make push-manifest VERSION=vX.Y.Z)
endif
	manifest-tool push from-args --platforms $(call join_platforms,$(ALL_ARCH)) --template calico/confd-ARCH:$(VERSION) --target calico/confd:$(VERSION)

release: clean
ifndef VERSION
	$(error VERSION is undefined - run using make release VERSION=vX.Y.Z)
endif
	git tag $(VERSION)

	# Check to make sure the tag isn't "-dirty".
	if git describe --tags --dirty | grep dirty; \
	then echo current git working tree is "dirty". Make sure you do not have any uncommitted changes ;false; fi

	# Build binary and docker image. 
	$(MAKE) all-container

	# Check that the version output includes the version specified.
	# Tests that the "git tag" makes it into the binaries. Main point is to catch "-dirty" builds
	# Release is currently supported on darwin / linux only.
	if ! docker run calico/confd /bin/confd --version | grep '$(VERSION)$$'; then \
	  echo "Reported version:" `docker run calico/confd /bin/confd --version` "\nExpected version: $(VERSION)"; \
	  false; \
	else \
	  echo "Version check passed\n"; \
	fi

	# Retag images with corect version and quay
	$(MAKE) all-tag-images

	# Check that images were created recently and that the IDs of the versioned and latest images match
	@docker images --format "{{.CreatedAt}}\tID:{{.ID}}\t{{.Repository}}:{{.Tag}}" calico/confd 
	@docker images --format "{{.CreatedAt}}\tID:{{.ID}}\t{{.Repository}}:{{.Tag}}" calico/confd:$(VERSION)

	@echo ""
	@echo "# Push the created tag to GitHub"
	@echo "  git push origin $(VERSION)"
	@echo ""
	@echo "# Now, create a GitHub release from the tag, add release notes, and attach the following binaries:"
	@echo ""
	@echo "- bin/confd"
	@echo ""
	@echo "# To find commit messages for the release notes:  git log --oneline <old_release_version>...$(VERSION)"
	@echo ""
	@echo "# Now push the newly created release images."
	@echo ""
	@echo "  make push-all VERSION=$(VERSION)"
	@echo "  make push-manifest VERSION=$(VERSION)"
	@echo ""
	@echo "# For the final release only, push the latest tag"
	@echo "# DO NOT PUSH THESE IMAGES FOR RELEASE CANDIDATES OR ALPHA RELEASES" 
	@echo ""
	@echo "  make push-all-latest"
	@echo "  make push-manifest VERSION=latest"
	@echo ""
	@echo "See RELEASING.md for detailed instructions."

.PHONY: clean
clean:
	rm -rf bin/*
	rm -rf tests/logs
	-docker rmi -f calico/confd
	-docker rmi -f quay.io/calico/confd
