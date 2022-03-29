package kubevirt

import (
	"bytes"
	"context"
	"fmt"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	kubevirtv1 "kubevirt.io/client-go/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ProviderName is the name of the kubevirt provider
	ProviderName = "kubevirt"
)

var scheme = runtime.NewScheme()

func init() {
	corev1.AddToScheme(scheme)
	kubevirtv1.AddToScheme(scheme)
}

type cloud struct {
	namespace            string
	client               client.Client
	managedClusterClient *kubernetes.Clientset
	config               CloudConfig
}

type CloudConfig struct {
	Kubeconfig   string             `yaml:"kubeconfig"` // The kubeconfig used to connect to the underkube
	LoadBalancer LoadBalancerConfig `yaml:"loadbalancer"`
	Instances    InstancesConfig    `yaml:"instances"`
	Zones        ZonesConfig        `yaml:"zones"`
}

type LoadBalancerConfig struct {
	Enabled              bool `yaml:"enabled"`              // Enables the loadbalancer interface of the CCM
	CreationPollInterval int  `yaml:"creationPollInterval"` // How many seconds to wait for the loadbalancer creation
}

type InstancesConfig struct {
	Enabled             bool `yaml:"enabled"`             // Enables the instances interface of the CCM
	EnableInstanceTypes bool `yaml:"enableInstanceTypes"` // Enables 'flavor' annotation to detect instance types
}

type ZonesConfig struct {
	Enabled bool `yaml:"enabled"` // Enables the zones interface of the CCM
}

// createDefaultCloudConfig creates a CloudConfig object filled with default values.
// These default values should be overwritten by values read from the cloud-config file.
func createDefaultCloudConfig() CloudConfig {
	return CloudConfig{
		LoadBalancer: LoadBalancerConfig{
			Enabled:              true,
			CreationPollInterval: defaultLoadBalancerCreatePollInterval,
		},
		Instances: InstancesConfig{
			Enabled: true,
		},
		Zones: ZonesConfig{
			Enabled: true,
		},
	}
}

func NewCloudConfigFromBytes(configBytes []byte) (CloudConfig, error) {
	var config = createDefaultCloudConfig()
	err := yaml.Unmarshal(configBytes, &config)
	if err != nil {
		return CloudConfig{}, err
	}
	return config, nil
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, kubevirtCloudProviderFactory)
}

func kubevirtCloudProviderFactory(config io.Reader) (cloudprovider.Interface, error) {
	if config == nil {
		return nil, fmt.Errorf("No %s cloud provider config file given", ProviderName)
	}

	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(config)
	if err != nil {
		return nil, fmt.Errorf("Failed to read cloud provider config: %v", err)
	}
	cloudConf, err := NewCloudConfigFromBytes(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("Failed to unmarshal cloud provider config: %v", err)
	}
	clientConfig, err := clientcmd.NewClientConfigFromBytes([]byte(cloudConf.Kubeconfig))
	if err != nil {
		return nil, err
	}
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	namespace, _, err := clientConfig.Namespace()
	if err != nil {
		klog.Errorf("Could not find namespace in client config: %v", err)
		return nil, err
	}
	c, err := client.New(restConfig, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, err
	}
	return &cloud{
		namespace: namespace,
		client:    c,
		config:    cloudConf,
	}, nil
}

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping activities within the cloud provider.
func (c *cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	clientBuilder.ConfigOrDie("")
	clientSet, err := kubernetes.NewForConfig(clientBuilder.ConfigOrDie(""))
	if err != nil {
		klog.Fatalf("Failed to intialize cloud provider client: %v", err)
	}

	c.managedClusterClient = clientSet
	go c.syncEP(context.TODO())
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	if !c.config.LoadBalancer.Enabled {
		return nil, false
	}

	return &loadbalancer{
		namespace:           c.namespace,
		cloudProviderClient: c.client,
		client:              c.managedClusterClient,
		config:              c.config.LoadBalancer,
	}, true
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) Instances() (cloudprovider.Instances, bool) {
	if !c.config.Instances.Enabled {
		return nil, false
	}
	return &instances{
		namespace: c.namespace,
		client:    c.client,
		config:    c.config.Instances,
	}, true
}

func (c *cloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return nil, false
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) Zones() (cloudprovider.Zones, bool) {
	if !c.config.Zones.Enabled {
		return nil, false
	}
	return &zones{
		namespace: c.namespace,
		client:    c.client,
	}, true
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (c *cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (c *cloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (c *cloud) ProviderName() string {
	return ProviderName
}

// HasClusterID returns true if a ClusterID is required and set
func (c *cloud) HasClusterID() bool {
	return true
}

func (c *cloud) syncEP(ctx context.Context) {
	watcher, err := c.managedClusterClient.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to create watcher for all pods: %v", err)
	}

	for event := range watcher.ResultChan() {
		switch event.Object.(type) {
		case *corev1.Pod:
			pod := event.Object.(*corev1.Pod)

			services, err := c.managedClusterClient.CoreV1().Services(pod.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(pod.Labels).String(),
			})
			if err != nil {
				klog.Error(err)
			}

			vmi := &kubevirtv1.VirtualMachineInstance{}
			if err := c.client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName, Namespace: c.namespace}, vmi); err != nil {
				klog.Error(err)
			}

			var (
				virtPods = &corev1.PodList{}
				virtPod  = corev1.Pod{}
			)
			if err := c.client.List(ctx, virtPods, nil); err != nil {
				klog.Error(err)
			}

			for _, p := range virtPods.Items {
				if p.Annotations["kubevirt.io/domain"] == vmi.Name {
					virtPod = p
					break
				}
			}

			for _, svc := range services.Items {
				if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
					continue
				}

				kvLBEndpoints := &corev1.Endpoints{}
				if err := c.client.Get(ctx,
					types.NamespacedName{Name: cloudprovider.DefaultLoadBalancerName(&svc), Namespace: c.namespace},
					kvLBEndpoints); err != nil {
					klog.Error(err)
				}

				for _, sub := range kvLBEndpoints.Subsets {
					for _, addr := range sub.Addresses {
						if addr.IP == pod.Status.PodIP {
							continue
						}

						ep := corev1.EndpointAddress{
							IP:       pod.Status.HostIP,
							NodeName: pointer.String(pod.Spec.NodeName),
							TargetRef: &corev1.ObjectReference{
								Kind:            "Pod",
								Name:            virtPod.Name,
								Namespace:       c.namespace,
								ResourceVersion: pod.ResourceVersion,
								UID:             pod.UID,
							},
						}

						sub.Addresses = append(sub.Addresses, ep)

					}
				}

				if err := c.client.Update(ctx, kvLBEndpoints); err != nil {
					klog.Error(err)
				}

			}

		default:
			klog.Warning("unexpected enqueued object: only objetcs of type Pod is expected")
		}
	}
}
