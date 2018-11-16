// Copyright (c) 2018 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package calico

import (
	"net"
	"os"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kelseyhightower/confd/pkg/resource/template"
)

const (
	envAdvertiseClusterIPs = "CALICO_ADVERTISE_CLUSTER_IPS"
	staticRoutesKey        = "calico/static-routes"
)

// routeGenerator defines the data fields
// necessary for monitoring the services/endpoints resources for
// valid service ips to advertise
type routeGenerator struct {
	sync.Mutex
	client                  *client
	nodeName                string
	svcInformer, epInformer cache.Controller
	svcIndexer, epIndexer   cache.Indexer
	svcRouteMap             map[string][]string // maps service name to ip
}

// NewRouteGenerator initializes a kube-api client and the informers
func NewRouteGenerator(c *client) (rg *routeGenerator, err error) {
	// initialize empty route generator
	rg = &routeGenerator{
		client:      c,
		nodeName:    template.NodeName,
		svcRouteMap: make(map[string][]string),
	}

	// set up k8s client
	// attempt 1: KUBECONFIG env var
	cfgFile := os.Getenv("KUBECONFIG")
	cfg, err := clientcmd.BuildConfigFromFlags("", cfgFile)
	if err != nil {
		log.WithError(err).Info("KUBECONFIG environment variable not found, attempting in-cluster")
		// attempt 2: in cluster config
		if cfg, err = rest.InClusterConfig(); err != nil {
			return
		}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return
	}

	// set up services informer
	svcWatcher := cache.NewListWatchFromClient(client.Core().RESTClient(), "services", "", fields.Everything())
	svcHandler := cache.ResourceEventHandlerFuncs{AddFunc: rg.onSvcAdd, UpdateFunc: rg.onSvcUpdate, DeleteFunc: rg.onSvcDelete}
	rg.svcIndexer, rg.svcInformer = cache.NewIndexerInformer(svcWatcher, &v1.Service{}, 0, svcHandler, cache.Indexers{})

	// set up endpoints informer
	epWatcher := cache.NewListWatchFromClient(client.Core().RESTClient(), "endpoints", "", fields.Everything())
	epHandler := cache.ResourceEventHandlerFuncs{AddFunc: rg.onEPAdd, UpdateFunc: rg.onEPUpdate, DeleteFunc: rg.onEPDelete}
	rg.epIndexer, rg.epInformer = cache.NewIndexerInformer(epWatcher, &v1.Endpoints{}, 0, epHandler, cache.Indexers{})

	return
}

// Start reads CALICO_STATIC_ROUTES for k8s-cluster-ips
// and CIDRs to advertise, comma-separated
func (rg *routeGenerator) Start(routeString string) {
	var (
		isDynamic bool
		ch        = make(chan struct{})
		cidrs     = []string{}
	)

	// parse routeString
	for _, route := range strings.Split(routeString, ",") {
		cidr := strings.TrimSpace(route)
		// consider anything else as cidr
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			log.WithField("cidr", cidr).WithError(err).Warn("Start: failed to parse cidr, passing")
			continue
		}
		cidrs = append(cidrs, cidr)
	}

	// affix cidrs to the route map
	rg.svcRouteMap[staticRoutesKey] = cidrs
	rg.updateRoutes()

	// start the listeners
	if isDynamic {
		go rg.svcInformer.Run(ch)
		go rg.epInformer.Run(ch)
	}

	return
}

// getServiceForEndpoints retrieves the corresponding svc for the given ep
func (rg *routeGenerator) getServiceForEndpoints(ep *v1.Endpoints) (*v1.Service, string) {
	// get key
	key, err := cache.MetaNamespaceKeyFunc(ep)
	if err != nil {
		log.WithField("ep", ep.Name).WithError(err).Warn("getServiceForEndpoints: error on retrieving key for endpoint, passing")
		return nil, ""
	}
	// get svc
	svcIface, exists, err := rg.svcIndexer.GetByKey(key)
	if err != nil {
		log.WithField("key", key).WithError(err).Warn("getServiceForEndpoints: error on retrieving service for key, passing")
		return nil, key
	} else if !exists {
		log.WithField("key", key).Debug("getServiceForEndpoints: service for key not found, passing")
		return nil, key
	}
	return svcIface.(*v1.Service), key
}

// getEndpointsForService retrieves the corresponding ep for the given svc
func (rg *routeGenerator) getEndpointsForService(svc *v1.Service) (*v1.Endpoints, string) {
	// get key
	key, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		log.WithField("svc", svc.Name).WithError(err).Warn("getEndpointsForService: error on retrieving key for service, passing")
		return nil, ""
	}
	// get ep
	epIface, exists, err := rg.epIndexer.GetByKey(key)
	if err != nil {
		log.WithField("key", key).WithError(err).Warn("getEndpointsForService: error on retrieving endpoint for key, passing")
		return nil, key
	} else if !exists {
		log.WithField("key", key).Debug("getEndpointsForService: service for endpoint not found, passing")
		return nil, key
	}
	return epIface.(*v1.Endpoints), key
}

// setRouteForSvc handles the main logic to check if a specified service or endpoint
// should have its route advertised by the node running this code
func (rg *routeGenerator) setRouteForSvc(svc *v1.Service, ep *v1.Endpoints) {

	// ensure both are not nil
	if svc == nil && ep == nil {
		log.Error("setRouteForSvc: both service and endpoint cannot be nil, passing...")
		return
	}

	var key string
	if svc == nil {
		// ep received but svc nil
		if svc, key = rg.getServiceForEndpoints(ep); svc == nil {
			return
		}
	} else if ep == nil {
		// svc received but ep nil
		if ep, key = rg.getEndpointsForService(svc); ep == nil {
			return
		}
	}

	// see if any endpoints are on this node and advertise if so
	// else remove the route if it also already exists
	rg.Lock()
	if rg.advertiseThisService(svc, ep) {
		rg.svcRouteMap[key] = []string{svc.Spec.ClusterIP + "/32"}
	} else if _, exists := rg.svcRouteMap[key]; exists {
		delete(rg.svcRouteMap, key)
	}
	rg.Unlock()

	rg.updateRoutes()
}

// advertiseThisService returns true if this service should be advertised on this node,
// false otherwise.
func (rg *routeGenerator) advertiseThisService(svc *v1.Service, ep *v1.Endpoints) bool {
	// do nothing if the svc is not a relevant type
	if (svc.Spec.Type != v1.ServiceTypeClusterIP) && (svc.Spec.Type != v1.ServiceTypeNodePort) && (svc.Spec.Type != v1.ServiceTypeLoadBalancer) {
		return false
	}

	// also do nothing if the clusterIP is empty or None
	if len(svc.Spec.ClusterIP) > 0 && svc.Spec.ClusterIP != "None" {
		return false
	}

	// always set if externalTrafficPolicy != local
	if svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeLocal {
		return true
	}

	// add svc clusterIP if node contains at least one endpoint for svc
	for _, subset := range ep.Subsets {
		// not interested in subset.NotReadyAddresses
		for _, address := range subset.Addresses {
			if address.NodeName == nil || *address.NodeName != rg.nodeName {
				continue
			}
			return true
		}
	}
	return false
}

// unsetRouteForSvc removes the route from the svcRouteMap
// but checks to see if it wasn't already deleted by its sibling resource
func (rg *routeGenerator) unsetRouteForSvc(obj interface{}) {
	// generate key
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.WithError(err).Warn("unsetRouteForSvc: error on retrieving key for object, passing")
		return
	}

	// mutex
	rg.Lock()
	defer rg.Unlock()

	// pass if already deleted
	if _, exists := rg.svcRouteMap[key]; !exists {
		return
	}

	// delete
	delete(rg.svcRouteMap, key)
	rg.updateRoutes()
}

// onSvcAdd is called when a k8s service is created
func (rg *routeGenerator) onSvcAdd(obj interface{}) {
	svc, ok := obj.(*v1.Service)
	if !ok {
		log.Warn("onSvcAdd: failed to assert type to service, passing")
		return
	}
	rg.setRouteForSvc(svc, nil)
}

// onSvcUpdate is called when a k8s service is updated
func (rg *routeGenerator) onSvcUpdate(_, obj interface{}) {
	svc, ok := obj.(*v1.Service)
	if !ok {
		log.Warn("onSvcUpdate: failed to assert type to service, passing")
		return
	}
	rg.setRouteForSvc(svc, nil)
}

// onSvcUpdate is called when a k8s service is deleted
func (rg *routeGenerator) onSvcDelete(obj interface{}) {
	rg.unsetRouteForSvc(obj)
}

// onEPAdd is called when a k8s endpoint is created
func (rg *routeGenerator) onEPAdd(obj interface{}) {
	ep, ok := obj.(*v1.Endpoints)
	if !ok {
		log.Warn("onEPAdd: failed to assert type to endpoints, passing")
		return
	}
	rg.setRouteForSvc(nil, ep)
}

// onEPUpdate is called when a k8s endpoint is updated
func (rg *routeGenerator) onEPUpdate(_, obj interface{}) {
	ep, ok := obj.(*v1.Endpoints)
	if !ok {
		log.Warn("onEPUpdate: failed to assert type to endpoints, passing")
		return
	}
	rg.setRouteForSvc(nil, ep)
}

// onEPDelete is called when a k8s endpoint is deleted
func (rg *routeGenerator) onEPDelete(obj interface{}) {
	rg.unsetRouteForSvc(obj)
}

// updateRoutes compiles all the svcIPs that have endpoints
// running on this node and calls the client's updateRoutes
func (rg *routeGenerator) updateRoutes() {
	log.WithField("svcRouteMap", rg.svcRouteMap).Debug("updateRoutes")
	cidrs := []string{}
	for _, ips := range rg.svcRouteMap {
		for _, ip := range ips {
			cidrs = append(cidrs, ip)
		}
	}
	rg.client.updateRoutes(cidrs)
	log.WithField("cidrs", cidrs).Debug("updateRoutes")
}
