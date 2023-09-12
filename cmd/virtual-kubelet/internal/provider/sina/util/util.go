package util

import (
	"context"
	"encoding/json"

	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/provider/sina/common"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/component-base/featuregate"
)

// GetRequestFromPod get resources required by pod
func GetRequestFromPod(pod *corev1.Pod) *common.Resource {
	if pod == nil {
		return nil
	}
	reqs, _ := PodRequestsAndLimits(pod)
	capacity := common.ConvertResource(reqs)
	return capacity
}

func ensureNamespace(ns string, client kubernetes.Interface, nsLister corelisters.NamespaceLister) error {
	_, err := nsLister.Get(ns)
	if err == nil {
		return nil
	}
	if !apierrs.IsNotFound(err) {
		return err
	}
	if _, err = client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		if !apierrs.IsAlreadyExists(err) {
			return err
		}
		return nil
	}

	return err
}

func filterPVC(pvcInSub *v1.PersistentVolumeClaim, hostIP string) error {
	labelSelector := pvcInSub.Spec.Selector.DeepCopy()
	pvcInSub.Spec.Selector = nil
	TrimObjectMeta(&pvcInSub.ObjectMeta)
	SetObjectGlobal(&pvcInSub.ObjectMeta)
	if labelSelector != nil {
		labelStr, err := json.Marshal(labelSelector)
		if err != nil {
			return err
		}
		pvcInSub.Annotations["labelSelector"] = string(labelStr)
	}
	if len(pvcInSub.Annotations[SelectedNodeKey]) != 0 {
		pvcInSub.Annotations[SelectedNodeKey] = hostIP
	}
	return nil
}

func filterPV(pvInSub *v1.PersistentVolume, hostIP string) {
	TrimObjectMeta(&pvInSub.ObjectMeta)
	if pvInSub.Annotations == nil {
		pvInSub.Annotations = make(map[string]string)
	}
	if pvInSub.Spec.NodeAffinity == nil {
		return
	}
	if pvInSub.Spec.NodeAffinity.Required == nil {
		return
	}
	terms := pvInSub.Spec.NodeAffinity.Required.NodeSelectorTerms
	for k, v := range pvInSub.Spec.NodeAffinity.Required.NodeSelectorTerms {
		mf := v.MatchFields
		me := v.MatchExpressions
		for k, val := range v.MatchFields {
			if val.Key == HostNameKey || val.Key == BetaHostNameKey {
				val.Values = []string{hostIP}
			}
			mf[k] = val
		}
		for k, val := range v.MatchExpressions {
			if val.Key == HostNameKey || val.Key == BetaHostNameKey {
				val.Values = []string{hostIP}
			}
			me[k] = val
		}
		terms[k].MatchFields = mf
		terms[k].MatchExpressions = me
	}
	pvInSub.Spec.NodeAffinity.Required.NodeSelectorTerms = terms
	return
}

func filterCommon(meta *metav1.ObjectMeta) error {
	TrimObjectMeta(meta)
	SetObjectGlobal(meta)
	return nil
}

func filterService(serviceInSub *v1.Service) error {
	labelSelector := serviceInSub.Spec.Selector
	serviceInSub.Spec.Selector = nil
	if serviceInSub.Spec.ClusterIP != "None" {
		serviceInSub.Spec.ClusterIP = ""
	}
	TrimObjectMeta(&serviceInSub.ObjectMeta)
	SetObjectGlobal(&serviceInSub.ObjectMeta)
	if labelSelector == nil {
		return nil
	}
	labelStr, err := json.Marshal(labelSelector)
	if err != nil {
		return err
	}
	serviceInSub.Annotations["labelSelector"] = string(labelStr)
	return nil
}

// CheckGlobalLabelEqual checks if two objects both has the global label
func CheckGlobalLabelEqual(obj, clone *metav1.ObjectMeta) bool {
	oldGlobal := IsObjectGlobal(obj)
	if !oldGlobal {
		return false
	}
	newGlobal := IsObjectGlobal(clone)
	if !newGlobal {
		return false
	}
	return true
}

// IsObjectGlobal return if an object is global
func IsObjectGlobal(obj *metav1.ObjectMeta) bool {
	if obj.Annotations == nil {
		return false
	}

	if obj.Annotations[GlobalLabel] == "true" {
		return true
	}

	return false
}

// SetObjectGlobal add global annotation to an object
func SetObjectGlobal(obj *metav1.ObjectMeta) {
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[GlobalLabel] = "true"
}

func PodRequestsAndLimits(pod *v1.Pod) (reqs, limits v1.ResourceList) {
	reqs, limits = v1.ResourceList{}, v1.ResourceList{}
	for _, container := range pod.Spec.Containers {
		addResourceList(reqs, container.Resources.Requests)
		addResourceList(limits, container.Resources.Limits)
	}
	// init containers define the minimum of any resource
	for _, container := range pod.Spec.InitContainers {
		maxResourceList(reqs, container.Resources.Requests)
		maxResourceList(limits, container.Resources.Limits)
	}

	// if PodOverhead feature is supported, add overhead for running a pod
	// to the sum of reqeuests and to non-zero limits:
	if pod.Spec.Overhead != nil && featuregate.NewFeatureGate().Enabled("PodOverhead") {
		addResourceList(reqs, pod.Spec.Overhead)

		for name, quantity := range pod.Spec.Overhead {
			if value, ok := limits[name]; ok && !value.IsZero() {
				value.Add(quantity)
				limits[name] = value
			}
		}
	}

	return
}

// addResourceList adds the resources in newList to list
func addResourceList(list, newList v1.ResourceList) {
	for name, quantity := range newList {
		if value, ok := list[name]; !ok {
			list[name] = quantity.DeepCopy()
		} else {
			value.Add(quantity)
			list[name] = value
		}
	}
}

// maxResourceList sets list to the greater of list/newList for every resource
// either list
func maxResourceList(list, new v1.ResourceList) {
	for name, quantity := range new {
		if value, ok := list[name]; !ok {
			list[name] = quantity.DeepCopy()
			continue
		} else {
			if quantity.Cmp(value) > 0 {
				list[name] = quantity.DeepCopy()
			}
		}
	}
}
