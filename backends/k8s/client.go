package k8s

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kelseyhightower/confd/log"
	backendapi "github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/compat"
	"github.com/projectcalico/libcalico-go/lib/backend/k8s/resources"
	"github.com/projectcalico/libcalico-go/lib/backend/k8s/thirdparty"
	"github.com/projectcalico/libcalico-go/lib/backend/model"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	clientapi "k8s.io/client-go/pkg/api"
	kapiv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ipPool         = "/calico/v1/ipam/v4/pool"
	global         = "/calico/bgp/v1/global"
	globalASN      = "/calico/bgp/v1/global/as_num"
	globalNodeMesh = "/calico/bgp/v1/global/node_mesh"
	allNodes       = "/calico/bgp/v1/host"
	globalLogging  = "/calico/bgp/v1/global/loglevel"
)

var (
	singleNode = regexp.MustCompile("^/calico/bgp/v1/host/([a-zA-Z0-9._-]*)$")
	ipBlock    = regexp.MustCompile("^/calico/ipam/v2/host/([a-zA-Z0-9._-]*)/ipv4/block")
)

type Client struct {
	clientSet            *kubernetes.Clientset
	tprClient            *rest.RESTClient
	nodesResourceVersion string

	nodeBgpPeerClient resources.K8sNodeResourceClient
	nodeBgpCfgClient  resources.K8sNodeResourceClient

	ipPoolClient        *watchClient
	globalBgpPeerClient *watchClient
	globalBgpCfgClient  *watchClient
	bgpNodeMeshClient   *watchClient
}

func NewK8sClient(kubeconfig string) (*Client, error) {

	log.Debug("Building k8s client")

	// Set an explicit path to the kubeconfig if one
	// was provided.
	loadingRules := clientcmd.ClientConfigLoadingRules{}
	if kubeconfig != "" {
		log.Debug(fmt.Sprintf("Using kubeconfig: \n%s", kubeconfig))
		loadingRules.ExplicitPath = kubeconfig
	}

	// A kubeconfig file was provided.  Use it to load a config, passing through
	// any overrides.
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}

	// Create the clientset
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	log.Debug(fmt.Sprintf("Created k8s clientSet: %+v", cs))

	tprClient, err := buildTPRClient(config)
	if err != nil {
		return nil, err
	}

	// We create clients for each of the custom resources that we need to watch.
	// The watchClient encapsulates details about the resource to enable us to list
	// and watch the resource.
	ipPoolClient := &watchClient{
		restClient:   tprClient,
		resourceName: resources.IPPoolResourceName,
		client:       resources.NewIPPoolClient(cs, tprClient),
		emptyList:    model.IPPoolListOptions{},
	}
	globalBgpPeerClient := &watchClient{
		restClient:   tprClient,
		resourceName: resources.GlobalBGPPeerResourceName,
		client:       resources.NewGlobalBGPPeerClient(cs, tprClient),
		emptyList:    model.GlobalBGPPeerListOptions{},
	}
	globalBgpCfgClient := &watchClient{
		restClient:   tprClient,
		resourceName: resources.GlobalBgpConfigResourceName,
		client:       resources.NewGlobalBGPConfigClient(cs, tprClient),
		emptyList:    model.GlobalBGPConfigListOptions{},
	}
	// The node-mesh watcher is just another global config watcher (since
	// we can't watch a single entry).  We track separately from the globalBgpCfgClient
	// because we need to keep the resource versions separate for the different path
	// roots tracked by confd.
	bgpNodeMeshClient := &watchClient{
		restClient:   tprClient,
		resourceName: resources.GlobalBgpConfigResourceName,
		client:       resources.NewGlobalBGPConfigClient(cs, tprClient),
		emptyList:    model.GlobalBGPConfigListOptions{},
	}

	kubeClient := &Client{
		clientSet:           cs,
		tprClient:           tprClient,
		ipPoolClient:        ipPoolClient,
		globalBgpPeerClient: globalBgpPeerClient,
		globalBgpCfgClient:  globalBgpCfgClient,
		bgpNodeMeshClient:   bgpNodeMeshClient,
		nodeBgpPeerClient:   resources.NewNodeBGPPeerClient(cs),
		nodeBgpCfgClient:    resources.NewNodeBGPConfigClient(cs),
	}

	return kubeClient, nil
}

// GetValues takes the etcd like keys and route it to the appropriate k8s API endpoint.
func (c *Client) GetValues(keys []string) (map[string]string, error) {
	var vars = make(map[string]string)
	for _, key := range keys {
		log.Debug(fmt.Sprintf("Getting key %s", key))
		if m := singleNode.FindStringSubmatch(key); m != nil {
			host := m[len(m)-1]
			kNode, err := c.clientSet.Nodes().Get(host, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			err = c.populateNodeDetails(kNode, vars)
			if err != nil {
				return nil, err
			}
			// Find the podCIDR assigned to individual Nodes
		} else if m := ipBlock.FindStringSubmatch(key); m != nil {
			host := m[len(m)-1]
			kNode, err := c.clientSet.Nodes().Get(host, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			cidr := kNode.Spec.PodCIDR
			parts := strings.Split(cidr, "/")
			cidr = strings.Join(parts, "-")
			vars[key+"/"+cidr] = "{}"
		}

		switch key {
		case global:
			// Set default values for fields that we always expect to have.
			vars[globalLogging] = "info"
			vars[globalASN] = "64512"
			vars[globalNodeMesh] = `{"enabled": true}`

			// Global data consists of both global config and global peers.
			kvps, _, err := c.globalBgpCfgClient.list()
			if err != nil {
				return nil, err
			}
			c.populateFromKVPairs(kvps, vars)

			kvps, _, err = c.globalBgpPeerClient.list()
			if err != nil {
				return nil, err
			}
			c.populateFromKVPairs(kvps, vars)
		case globalNodeMesh:
			// This is needed as there are calls to 'global' and directly to 'global/node_mesh'
			// Default to true, but we may override this if a value is configured.
			vars[globalNodeMesh] = `{"enabled": true}`

			// Get the configured value.
			kvps, _, err := c.globalBgpCfgClient.list()
			if err != nil {
				return nil, err
			}
			c.populateFromKVPairs(kvps, vars)
		case ipPool:
			kvps, _, err := c.ipPoolClient.list()
			if err != nil {
				return nil, err
			}
			c.populateFromKVPairs(kvps, vars)
		case allNodes:
			nodes, err := c.clientSet.Nodes().List(metav1.ListOptions{})
			if err != nil {
				return nil, err
			}

			for _, kNode := range nodes.Items {
				err := c.populateNodeDetails(&kNode, vars)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	log.Debug(fmt.Sprintf("%v", vars))
	return vars, nil
}

func (c *Client) WatchPrefix(prefix string, keys []string, waitIndex uint64, stopChan chan bool) (uint64, error) {

	// Kubernetes uses a string resource version, so rather than converting to a uint64, just store
	// in our private data the current revision for each prefix.  We'll use a wait index of 0 to
	// indicate a List is required, and a value of 1 to indicate a Watch is required.

	if waitIndex == 0 {
		switch prefix {
		case global:
			// Global path consists of both BGP config and BGP Peers.
			if err := c.globalBgpCfgClient.updateVersion(); err != nil {
				return 0, err
			}
			if err := c.globalBgpPeerClient.updateVersion(); err != nil {
				return 0, err
			}
			return 1, nil
		case globalNodeMesh:
			// Global node mesh is a specific bgp config option, but we can only watch on all
			// global BGP config.
			if err := c.bgpNodeMeshClient.updateVersion(); err != nil {
				return 0, err
			}
			return 1, nil
		case ipPool:
			// Global node mesh is a specific bgp config option, but we can only watch on all
			// global BGP config.
			if err := c.ipPoolClient.updateVersion(); err != nil {
				return 0, err
			}
			return 1, nil
		case allNodes:
			// Get all nodes.  The k8s client does not expose a way to watch a single Node.
			nodes, err := c.clientSet.Nodes().List(metav1.ListOptions{})
			if err != nil {
				return 0, err
			}
			c.nodesResourceVersion = nodes.ListMeta.ResourceVersion
			return 1, nil
		default:
			// We aren't tracking this key, default to 60 second refresh.
			time.Sleep(60 * time.Second)
			log.Debug(fmt.Sprintf("Receieved unknown key: %v", prefix))
			return 0, nil
		}
	}

	switch prefix {
	case global:
		if err := c.waitForEventOnTwo(c.globalBgpCfgClient, c.globalBgpPeerClient); err != nil {
			return 0, err
		}
		return 1, nil
	case globalNodeMesh:
		if err := c.waitForEvent(c.bgpNodeMeshClient); err != nil {
			return 0, err
		}
		return 1, nil
	case ipPool:
		if err := c.waitForEvent(c.ipPoolClient); err != nil {
			return 0, err
		}
		return 1, nil
	case allNodes:
		w, err := c.clientSet.Nodes().Watch(metav1.ListOptions{
			ResourceVersion: c.nodesResourceVersion,
		})
		if err != nil {
			return 0, err
		}
		event := <-w.ResultChan()
		ver := event.Object.(*kapiv1.NodeList).ListMeta.ResourceVersion
		w.Stop()
		log.Debug(fmt.Sprintf("All nodes resource version: %s", ver))
		c.nodesResourceVersion = ver
		return 1, nil
	default:
		// We aren't tracking this key, default to 60 second refresh.
		time.Sleep(60 * time.Second)
		log.Debug(fmt.Sprintf("Receieved unknown key: %v", prefix))
		return 1, nil
	}
	return waitIndex, nil
}

// buildTPRClient builds a RESTClient configured to interact with Calico ThirdPartyResources.
func buildTPRClient(baseConfig *rest.Config) (*rest.RESTClient, error) {
	// Generate config using the base config.
	cfg := baseConfig
	cfg.GroupVersion = &schema.GroupVersion{
		Group:   "projectcalico.org",
		Version: "v1",
	}
	cfg.APIPath = "/apis"
	cfg.ContentType = runtime.ContentTypeJSON
	cfg.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: clientapi.Codecs}

	cli, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, err
	}

	// We also need to register resources.
	schemeBuilder := runtime.NewSchemeBuilder(
		func(scheme *runtime.Scheme) error {
			scheme.AddKnownTypes(
				*cfg.GroupVersion,
				&thirdparty.GlobalConfig{},
				&thirdparty.GlobalConfigList{},
				&thirdparty.IpPool{},
				&thirdparty.IpPoolList{},
				&thirdparty.GlobalBgpPeer{},
				&thirdparty.GlobalBgpPeerList{},
			)
			return nil
		})
	schemeBuilder.AddToScheme(clientapi.Scheme)

	return cli, nil
}

// populateNodeDetails populates the given kvps map with values we track from the k8s Node object.
func (c *Client) populateNodeDetails(kNode *kapiv1.Node, vars map[string]string) error {
	kvps := []*model.KVPair{}

	// Start with the main Node configuration
	cNode, err := resources.K8sNodeToCalico(kNode)
	if err != nil {
		log.Error("Failed to parse k8s Node into Calico Node")
		return err
	}
	kvps = append(kvps, cNode)

	// Add per-node BGP config (each of the per-node resource clients also implements
	// the CustomK8sNodeResourceList interface, used to extract per-node resources from
	// the Node resource.
	if cfg, err := c.nodeBgpCfgClient.ExtractResourcesFromNode(kNode); err != nil {
		log.Error("Failed to parse BGP configs from node resource - skip config data")
	} else {
		kvps = append(kvps, cfg...)
	}

	if peers, err := c.nodeBgpPeerClient.ExtractResourcesFromNode(kNode); err != nil {
		log.Error("Failed to parse BGP peers from node resource - skip config data")
	} else {
		kvps = append(kvps, peers...)
	}

	// Populate the vars map from the KVPairs.
	c.populateFromKVPairs(kvps, vars)

	return nil
}

// populateFromKVPairs populates the vars KV map from the supplied set of
// KVPairs.  This uses the libcalico-go compat module and serialization functions
// to write out the KVPairs in etcdv2 format.  This works in conjunction with the
// etcdVarClient defined below which provides a "mock" etcd backend which actually
// just writes out data to the vars map.
func (c *Client) populateFromKVPairs(kvps []*model.KVPair, vars map[string]string) {
	// Create a etcdVarClient to write the KVP results in the vars map, using the
	// compat adaptor to write the values in etcdv2 format.
	client := compat.NewAdaptor(&etcdVarClient{vars: vars})
	for _, kvp := range kvps {
		client.Apply(kvp)
	}
}

// waitForEvent waits for an event on the supplied watchClients and returns as soon
// as an event occurs.
func (c *Client) waitForEvent(wc *watchClient) error {
	w, err := wc.watch()
	if err != nil {
		return err
	}
	defer w.Stop()
	event := <-w.ResultChan()
	if event.Type == watch.Error {
		return fmt.Errorf("Error watching resource")
	}

	// Extract the resource version from the event object (all Calico custom resource types
	// implement the ObjectMetaAccessor interface).
	wc.resourceVersion = event.Object.(metav1.ObjectMetaAccessor).GetObjectMeta().GetResourceVersion()
	log.Debug(fmt.Sprintf("Resource version: %s", wc.resourceVersion))
	return nil
}

// waitForEventOnTwo waits for an event on either of the two supplied watchClients
// and returns as soon as an event occurs.
func (c *Client) waitForEventOnTwo(wc1 *watchClient, wc2 *watchClient) error {
	w1, err := wc1.watch()
	if err != nil {
		return err
	}
	defer w1.Stop()
	w2, err := wc2.watch()
	if err != nil {
		return err
	}
	defer w2.Stop()

	// Wait for an event on either channel and keep track of which client the event was from.
	var event watch.Event
	var wc *watchClient
	select {
	case event = <-w1.ResultChan():
		wc = wc1
	case event = <-w2.ResultChan():
		wc = wc2
	}

	if event.Type == watch.Error {
		return fmt.Errorf("Error watching resource")
	}

	// Extract the resource version from the event object (all Calico custom resource types
	// implement the ObjectMetaAccessor interface) and update the stored version.
	wc.resourceVersion = event.Object.(metav1.ObjectMetaAccessor).GetObjectMeta().GetResourceVersion()
	log.Debug(fmt.Sprintf("Resource version: %s", wc.resourceVersion))
	return nil
}

type watchClient struct {
	restClient      *rest.RESTClient
	resourceName    string
	client          resources.K8sResourceClient
	emptyList       model.ListInterface
	resourceVersion string
}

func (w *watchClient) list() ([]*model.KVPair, string, error) {
	return w.client.List(w.emptyList)
}

func (w *watchClient) watch() (watch.Interface, error) {
	return cache.NewListWatchFromClient(
		w.restClient,
		w.resourceName,
		"kube-system",
		fields.Everything(),
	).WatchFunc(metav1.ListOptions{ResourceVersion: w.resourceVersion})

}

func (w *watchClient) updateVersion() error {
	_, ver, err := w.list()
	if err != nil {
		return err
	}
	w.resourceVersion = ver
	return nil
}

// etcdVarClient implements the libcalico-go api.Client interface.  It is used to
// write the KVPairs retrieved from the Kubernetes datastore driver into the KV map
// using etcdv2 naming scheme.
//
// By using the compat module (which adjusts for the etcdv2 API) and the serialization
// methods that are part of libcalico-go, we can get the etcdv2-equivalent keys and paths.
type etcdVarClient struct {
	vars map[string]string
}

func (c *etcdVarClient) Create(kvp *model.KVPair) (*model.KVPair, error) {
	log.Fatal("Create should not be invoked")
	return nil, nil
}

func (c *etcdVarClient) Update(kvp *model.KVPair) (*model.KVPair, error) {
	log.Fatal("Update should not be invoked")
	return nil, nil
}

func (c *etcdVarClient) Apply(kvp *model.KVPair) (*model.KVPair, error) {
	path, err := model.KeyToDefaultPath(kvp.Key)
	if err != nil {
		log.Error("Unable to create path from Key: %s", kvp.Key)
		return nil, err
	}
	value, err := model.SerializeValue(kvp)
	if err != nil {
		log.Error("Unable to serialize value: %s", kvp.Key)
		return nil, err
	}
	c.vars[path] = string(value)
	return kvp, nil
}

func (c *etcdVarClient) Delete(kvp *model.KVPair) error {
	log.Debug("Delete ignored")
	return nil
}

func (c *etcdVarClient) Get(key model.Key) (*model.KVPair, error) {
	log.Fatal("Get should not be invoked")
	return nil, nil
}

func (c *etcdVarClient) List(list model.ListInterface) ([]*model.KVPair, error) {
	log.Fatal("List should not be invoked")
	return nil, nil
}

func (c *etcdVarClient) Syncer(callbacks backendapi.SyncerCallbacks) backendapi.Syncer {
	log.Fatal("Syncer should not be invoked")
	return nil
}

func (c *etcdVarClient) EnsureInitialized() error {
	log.Fatal("EnsureIntialized should not be invoked")
	return nil
}

func (c *etcdVarClient) EnsureCalicoNodeInitialized(node string) error {
	log.Fatal("EnsureCalicoNodeInitialized should not be invoked")
	return nil
}
