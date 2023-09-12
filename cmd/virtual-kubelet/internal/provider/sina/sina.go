package sina

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/commands/root"
	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/provider"
	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/provider/sina/common"
	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/provider/sina/util"
	"github.com/virtual-kubelet/virtual-kubelet/internal/manager"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kubeinformers "k8s.io/client-go/informers"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

const (
	// Provider configuration defaults.
	defaultCPUCapacity    = "20"
	defaultMemoryCapacity = "100Gi"
	defaultPodCapacity    = "20"

	// Values used in tracing as attribute keys.
	namespaceKey     = "namespace"
	nameKey          = "name"
	containerNameKey = "containerName"
)

// See: https://github.com/virtual-kubelet/virtual-kubelet/issues/632
/*
var (
	_ providers.Provider           = (*SinaV0Provider)(nil)
	_ providers.PodMetricsProvider = (*SinaV0Provider)(nil)
	_ node.PodNotifier         = (*SinaProvider)(nil)
)
*/

// SinaProvider implements the virtual-kubelet provider interface and stores pods in memory.
type ClientConfig struct {
	// allowed qps of the kube client
	KubeClientQPS int
	// allowed burst of the kube client
	KubeClientBurst int
	// config path of the kube client
	ClientKubeConfigPath string
}

type clientCache struct {
	podLister    listersv1.PodLister
	nsLister     listersv1.NamespaceLister
	cmLister     listersv1.ConfigMapLister
	secretLister listersv1.SecretLister
	nodeLister   listersv1.NodeLister
}

type SinaProvider struct { //nolint:golint
	master               kubernetes.Interface
	client               kubernetes.Interface
	metricClient         versioned.Interface
	config               *rest.Config
	daemonPort           int32
	nodeName             string
	version              string
	ignoreLabels         []string
	clientCache          clientCache
	rm                   *manager.ResourceManager
	updatedNode          chan *corev1.Node
	updatedPod           chan *corev1.Pod
	enableServiceAccount bool
	stopCh               <-chan struct{}
	providerNode         *common.ProviderNode
	configured           bool
}

// SinaConfig contains a Sina virtual-kubelet's configurable parameters.
type SinaConfig struct { //nolint:golint
	CPU          string            `json:"cpu,omitempty"`
	Memory       string            `json:"memory,omitempty"`
	Pods         string            `json:"pods,omitempty"`
	Others       map[string]string `json:"others,omitempty"`
	ProviderID   string            `json:"providerID,omitempty"`
	Clientconfig string            `json:"clientconfig,omitempty"`
}

// NewSinaProvider creates a new SinaProvider, which implements the PodNotifier interface
func NewSinaProvider(providerConfig provider.InitConfig, cc *ClientConfig,
	ignoreLabelsStr string, enableServiceAccount bool, opts *root.Opts) (*SinaProvider, error) {
	ignoreLabels := strings.Split(ignoreLabelsStr, ",")
	if home := homedir.HomeDir(); home != "" {
		cc.ClientKubeConfigPath = filepath.Join(home, ".kube", "config-1")
	}
	// client config
	var clientConfig *rest.Config
	client, err := util.NewClient(cc.ClientKubeConfigPath, func(config *rest.Config) {
		config.QPS = float32(cc.KubeClientQPS)
		config.Burst = cc.KubeClientBurst
		// Set config for clientConfig
		clientConfig = config
	})
	if err != nil {
		return nil, fmt.Errorf("could not build clientset for cluster: %v", err)
	}

	// master config, maybe a real node or a pod
	master, err := util.NewClient(providerConfig.ConfigPath, func(config *rest.Config) {
		config.QPS = float32(opts.KubeAPIQPS)
		config.Burst = int(opts.KubeAPIBurst)
	})
	if err != nil {
		return nil, fmt.Errorf("could not build clientset for cluster: %v", err)
	}

	metricClient, err := util.NewMetricClient(cc.ClientKubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("could not build clientset for cluster: %v", err)
	}

	serverVersion, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("could not get target cluster server version: %v", err)
	}

	informer := kubeinformers.NewSharedInformerFactory(client, 0)
	podInformer := informer.Core().V1().Pods()
	nsInformer := informer.Core().V1().Namespaces()
	nodeInformer := informer.Core().V1().Nodes()
	cmInformer := informer.Core().V1().ConfigMaps()
	secretInformer := informer.Core().V1().Secrets()

	ctx := context.TODO()

	sinaProvider := &SinaProvider{
		master:               master,
		client:               client,
		metricClient:         metricClient,
		nodeName:             providerConfig.NodeName,
		ignoreLabels:         ignoreLabels,
		version:              serverVersion.GitVersion,
		daemonPort:           providerConfig.DaemonPort,
		config:               clientConfig,
		enableServiceAccount: enableServiceAccount,
		clientCache: clientCache{
			podLister:    podInformer.Lister(),
			nsLister:     nsInformer.Lister(),
			cmLister:     cmInformer.Lister(),
			secretLister: secretInformer.Lister(),
			nodeLister:   nodeInformer.Lister(),
		},
		rm:           providerConfig.ResourceManager,
		updatedNode:  make(chan *corev1.Node, 100),
		updatedPod:   make(chan *corev1.Pod, 100000),
		providerNode: &common.ProviderNode{},
		stopCh:       ctx.Done(),
	}

	sinaProvider.buildNodeInformer(nodeInformer)
	sinaProvider.buildPodInformer(podInformer)

	informer.Start(ctx.Done())
	klog.Info("Informer started")
	if !cache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced,
		nsInformer.Informer().HasSynced, nodeInformer.Informer().HasSynced, cmInformer.Informer().HasSynced,
		secretInformer.Informer().HasSynced) {
		klog.Fatal("WaitForCacheSync failed")
	}
	return sinaProvider, nil
}

// GetClient return the kube client of lower cluster
func (v *SinaProvider) GetClient() kubernetes.Interface {
	return v.client
}

// GetMaster return the kube client of upper cluster
func (v *SinaProvider) GetMaster() kubernetes.Interface {
	return v.master
}

// GetNameSpaceLister returns the namespace cache
func (v *SinaProvider) GetNameSpaceLister() listersv1.NamespaceLister {
	return v.clientCache.nsLister
}

func (v *SinaProvider) buildNodeInformer(nodeInformer informerv1.NodeInformer) {
	nodeInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if !v.configured {
					return
				}
				nodeCopy := v.providerNode.DeepCopy()
				addNode := obj.(*corev1.Node).DeepCopy()
				toAdd := common.ConvertResource(addNode.Status.Capacity)
				if err := v.providerNode.AddResource(toAdd); err != nil {
					return
				}
				// resource we did not add when ConfigureNode should sub
				v.providerNode.SubResource(v.getResourceFromPodsByNodeName(addNode.Name))
				copy := v.providerNode.DeepCopy()
				if !reflect.DeepEqual(nodeCopy, copy) {
					v.updatedNode <- copy
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if !v.configured {
					return
				}
				old, ok1 := oldObj.(*corev1.Node)
				new, ok2 := newObj.(*corev1.Node)
				oldCopy := old.DeepCopy()
				newCopy := new.DeepCopy()
				if !ok1 || !ok2 {
					return
				}
				klog.V(5).Infof("Node %v updated", old.Name)
				v.updateVKCapacityFromNode(oldCopy, newCopy)
			},
			DeleteFunc: func(obj interface{}) {
				if !v.configured {
					return
				}
				nodeCopy := v.providerNode.DeepCopy()
				deleteNode := obj.(*corev1.Node).DeepCopy()
				toRemove := common.ConvertResource(deleteNode.Status.Capacity)
				if err := v.providerNode.SubResource(toRemove); err != nil {
					return
				}
				// resource we did not add when ConfigureNode should add
				v.providerNode.AddResource(v.getResourceFromPodsByNodeName(deleteNode.Name))
				copy := v.providerNode.DeepCopy()
				if !reflect.DeepEqual(nodeCopy, copy) {
					v.updatedNode <- copy
				}
			},
		},
	)
}

func (v *SinaProvider) buildPodInformer(podInformer informerv1.PodInformer) {
	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    v.addPod,
			UpdateFunc: v.updatePod,
			DeleteFunc: v.deletePod,
		},
	)
}

func (v *SinaProvider) addPod(obj interface{}) {
	if !v.configured {
		return
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	podCopy := pod.DeepCopy()
	util.TrimObjectMeta(&podCopy.ObjectMeta)
	if !util.IsVirtualPod(podCopy) {
		if v.providerNode.Node == nil {
			return
		}
		// Pod created only by lower cluster
		// we should change the node resource
		if len(podCopy.Spec.NodeName) != 0 {
			podResource := util.GetRequestFromPod(podCopy)
			podResource.Pods = resource.MustParse("1")
			v.providerNode.SubResource(podResource)
			klog.Infof("Lower cluster add pod %s, resource: %v, node: %v",
				podCopy.Name, podResource, v.providerNode.Status.Capacity)
			if v.providerNode.Node == nil {
				return
			}
			copy := v.providerNode.DeepCopy()
			v.updatedNode <- copy
		}
		return
	}
	v.updatedPod <- podCopy
}

func (v *SinaProvider) updatePod(oldObj, newObj interface{}) {
	if !v.configured {
		return
	}
	old, ok1 := oldObj.(*corev1.Pod)
	new, ok2 := newObj.(*corev1.Pod)
	oldCopy := old.DeepCopy()
	newCopy := new.DeepCopy()
	if !ok1 || !ok2 {
		return
	}
	if !util.IsVirtualPod(newCopy) {
		// Pod created only by lower cluster
		// we should change the node resource
		if v.providerNode.Node == nil {
			return
		}
		v.updateVKCapacityFromPod(oldCopy, newCopy)
		return
	}
	// when pod deleted in lower cluster
	// set DeletionGracePeriodSeconds to nil because it's readOnly
	if newCopy.DeletionTimestamp != nil {
		newCopy.DeletionGracePeriodSeconds = nil
	}

	if !reflect.DeepEqual(oldCopy.Status, newCopy.Status) || newCopy.DeletionTimestamp != nil {
		util.TrimObjectMeta(&newCopy.ObjectMeta)
		v.updatedPod <- newCopy
	}
}

func (v *SinaProvider) deletePod(obj interface{}) {
	if !v.configured {
		return
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	podCopy := pod.DeepCopy()
	util.TrimObjectMeta(&podCopy.ObjectMeta)
	if !util.IsVirtualPod(podCopy) {
		if v.providerNode.Node == nil {
			return
		}
		if podStopped(pod) {
			return
		}
		// Pod created only by lower cluster
		// we should change the node resource
		if len(podCopy.Spec.NodeName) != 0 {
			podResource := util.GetRequestFromPod(podCopy)
			podResource.Pods = resource.MustParse("1")
			v.providerNode.AddResource(podResource)
			klog.Infof("Lower cluster add pod %s, resource: %v, node: %v",
				podCopy.Name, podResource, v.providerNode.Status.Capacity)
			if v.providerNode.Node == nil {
				return
			}
			copy := v.providerNode.DeepCopy()
			v.updatedNode <- copy
		}
		return
	}
	v.updatedPod <- podCopy
}

func (v *SinaProvider) updateVKCapacityFromNode(old, new *corev1.Node) {
	oldStatus, newStatus := compareNodeStatusReady(old, new)
	if !oldStatus && !newStatus {
		return
	}
	toRemove := common.ConvertResource(old.Status.Capacity)
	toAdd := common.ConvertResource(new.Status.Capacity)
	nodeCopy := v.providerNode.DeepCopy()
	if old.Spec.Unschedulable && !new.Spec.Unschedulable || newStatus && !oldStatus {
		v.providerNode.AddResource(toAdd)
		v.providerNode.SubResource(v.getResourceFromPodsByNodeName(old.Name))
	}
	if !old.Spec.Unschedulable && new.Spec.Unschedulable || oldStatus && !newStatus {
		v.providerNode.AddResource(v.getResourceFromPodsByNodeName(old.Name))
		v.providerNode.SubResource(toRemove)

	}
	if !reflect.DeepEqual(old.Status.Allocatable, new.Status.Allocatable) ||
		!reflect.DeepEqual(old.Status.Capacity, new.Status.Capacity) {
		klog.Infof("Start to update node resource, old: %v, new %v", old.Status.Capacity,
			new.Status.Capacity)
		v.providerNode.AddResource(toAdd)
		v.providerNode.SubResource(toRemove)
		klog.Infof("Current node resource, resource: %v, allocatable %v", v.providerNode.Status.Capacity,
			v.providerNode.Status.Allocatable)
	}
	if v.providerNode.Node == nil {
		return
	}
	copy := v.providerNode.DeepCopy()
	if !reflect.DeepEqual(nodeCopy, copy) {
		v.updatedNode <- copy
	}
}

func (v *SinaProvider) updateVKCapacityFromPod(old, new *corev1.Pod) {
	newResource := util.GetRequestFromPod(new)
	oldResource := util.GetRequestFromPod(old)
	// create pod
	if old.Spec.NodeName == "" && new.Spec.NodeName != "" {
		newResource.Pods = resource.MustParse("1")
		v.providerNode.SubResource(newResource)
		klog.Infof("Lower cluster add pod %s, resource: %v, node: %v",
			new.Name, newResource, v.providerNode.Status.Capacity)
	}
	// delete pod
	if old.Status.Phase == corev1.PodRunning && podStopped(new) {
		klog.Infof("Lower cluster delete pod %s, resource: %v", new.Name, newResource)
		newResource.Pods = resource.MustParse("1")
		v.providerNode.AddResource(newResource)
	}
	// update pod
	if new.Status.Phase == corev1.PodRunning && !reflect.DeepEqual(old.Spec.Containers,
		new.Spec.Containers) {
		if oldResource.Equal(newResource) {
			return
		}
		v.providerNode.AddResource(oldResource)
		v.providerNode.SubResource(newResource)

		klog.Infof("Lower cluster update pod %s, oldResource: %v, newResource: %v",
			new.Name, oldResource, newResource)
	}
	if v.providerNode.Node == nil {
		return
	}
	copy := v.providerNode.DeepCopy()
	v.updatedNode <- copy
	return
}
