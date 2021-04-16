/*
 * Tencent is pleased to support the open source community by making TKEStack available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use
 * this file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

package tappupdate

import (
	"encoding/json"
	"fmt"
	"strconv"

	tappv1 "tkestack.io/tapp/pkg/apis/tappcontroller/v1"
	clientset "tkestack.io/tapp/pkg/client/clientset/versioned"
	informers "tkestack.io/tapp/pkg/client/informers/externalversions"
	listers "tkestack.io/tapp/pkg/client/listers/tappcontroller/v1"
	"tkestack.io/tapp/pkg/hash"
	"tkestack.io/tapp/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

const (
	controllerName              = "tapp-controller-check"
	oldVersionTemplateHashKey   = "tkestack.io/oldVersion-templateHash-state"
	oldVersionTemplateHashValue = "not-changed"
)

var (
	deletePodAfterAppFinish = false
)

// Controller is the controller implementation for TApp resources
type Controller struct {
	// kubeclient is a standard kubernetes clientset
	kubeclient kubernetes.Interface
	// tappclient is a clientset for our own API group
	tappclient clientset.Interface

	tappLister  listers.TAppLister
	tappsSynced cache.InformerSynced

	tappHash hash.TappHashInterface

	// podStore is a cache of watched pods.
	podStore corelisters.PodLister

	// podStoreSynced returns true if the pod store has synced at least once.
	podStoreSynced cache.InformerSynced

	isOldVersion bool
}

// NewController returns a new tapp controller
func NewController(
	kubeclientset kubernetes.Interface,
	tappclientset clientset.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory,
	tappInformerFactory informers.SharedInformerFactory,
	isOldVersion bool) *Controller {

	// obtain references to shared index informers for TApp and pod types.
	tappInformer := tappInformerFactory.Tappcontroller().V1().TApps()
	podInformer := kubeInformerFactory.Core().V1().Pods()

	controller := &Controller{
		kubeclient:  kubeclientset,
		tappclient:  tappclientset,
		tappLister:  tappInformer.Lister(),
		tappsSynced: tappInformer.Informer().HasSynced,
		tappHash:    hash.NewTappHash(),
	}

	klog.Info("Setting up event handlers")

	controller.podStore = podInformer.Lister()

	controller.podStoreSynced = podInformer.Informer().HasSynced

	controller.isOldVersion = isOldVersion
	return controller
}

func (c *Controller) Run(interval int, stopCh <-chan struct{}) error {
	klog.Info("Starting tapp update")
	if ok := cache.WaitForCacheSync(stopCh, c.podStoreSynced, c.tappsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")

	tapplist, err := c.tappLister.TApps("").List(labels.Everything())
	if err != nil {
		return err
	}
	for _, tapp := range tapplist {
		err = c.sync(tapp)
		if err != nil {
			return err
		}
	}
	return nil
}

// getPodsForTApps returns the pods that match the selectors of the given tapp.
func (c *Controller) getPodsForTApp(tapp *tappv1.TApp) ([]*corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(tapp.Spec.Selector)
	if err != nil {
		return []*corev1.Pod{}, err
	}
	pods, err := c.podStore.Pods(tapp.Namespace).List(sel)
	if err != nil {
		return []*corev1.Pod{}, err
	}
	result := make([]*corev1.Pod, 0, len(pods))
	for i := range pods {
		controllerRef := metav1.GetControllerOf(pods[i])
		if controllerRef == nil {
			continue
		}
		if controllerRef.UID == tapp.UID {
			result = append(result, pods[i].DeepCopy())
		} else {
			klog.V(4).Infof("pod %s in namespace %s matches the selector of tapp %s, but has different controllerRef",
				pods[i].Name, pods[i].Namespace, util.GetTAppFullName(tapp))
		}
	}
	return result, nil
}

func (c *Controller) sync(tapp *tappv1.TApp) error {
	pods, err := c.getPodsForTApp(tapp)
	if err != nil {
		klog.Errorf("Failed to get pods for tapp %s: %v", util.GetTAppFullName(tapp), err)
		return err
	}
	if isTAppFinished(tapp) && tapp.Generation == tapp.Status.ObservedGeneration &&
		tapp.Spec.Replicas == tapp.Status.Replicas && len(pods) == 0 {
		klog.Errorf("Tapp %s has finished, replica: %d, status: %s", util.GetTAppFullName(tapp),
			tapp.Spec.Replicas, tapp.Status.AppStatus)
		return nil
	}
	c.syncTApp(tapp, pods)
	return nil
}

func (c *Controller) syncTApp(tapp *tappv1.TApp, pods []*corev1.Pod) error {
	tapp = tapp.DeepCopy()
	c.updateTemplateHash(tapp)
	podMap := makePodMap(pods)

	desiredRunningPods := getDesiredInstance(tapp)
	c.syncRunningPods(tapp, desiredRunningPods, podMap)
	return nil
}

// updateTemplateHash will generate and update templates hash if needed.
func (c *Controller) updateTemplateHash(tapp *tappv1.TApp) {
	updateHash := func(template *corev1.PodTemplateSpec) {
		if c.tappHash.SetTemplateHash(template) {
			c.tappHash.SetUniqHash(template)
		}
	}

	updateHash(&tapp.Spec.Template)

	for _, template := range tapp.Spec.TemplatePool {
		updateHash(&template)
	}
}

func (c *Controller) syncRunningPods(tapp *tappv1.TApp, desiredRunningPods sets.String,
	podMap map[string]*corev1.Pod) {
	for _, id := range desiredRunningPods.List() {
		if pod, ok := podMap[id]; ok {
			if c.isOldVersion && !c.isTemplateHashChanged(tapp, id, pod) {
				c.setOldVersionTemplateHashNotChanged(pod)
			} else if !c.isOldVersion && c.isOldVersionTemplateHashNotChanged(pod) {
				c.updatePodHash(tapp, id, pod)
			}
		}
	}
	return
}

func (c *Controller) isTemplateHashChanged(tapp *tappv1.TApp, podId string, pod *corev1.Pod) bool {
	hash := c.tappHash.GetTemplateHash(pod.Labels)

	template, err := getPodTemplate(&tapp.Spec, podId)
	if err != nil {
		klog.Errorf("Failed to get pod template for %s from tapp %s", getPodFullName(pod),
			util.GetTAppFullName(tapp))
		return true
	}
	expected := c.tappHash.GetTemplateHash(template.Labels)
	return hash != expected
}

func (c *Controller) isOldVersionTemplateHashNotChanged(pod *corev1.Pod) bool {
	stateStr, ok := pod.Annotations[oldVersionTemplateHashKey]
	if !ok || stateStr != oldVersionTemplateHashValue {
		return false
	}
	return true
}

func (c *Controller) setOldVersionTemplateHashNotChanged(pod *corev1.Pod) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[oldVersionTemplateHashKey] = oldVersionTemplateHashValue
	patchData := map[string]interface{}{"metadata": map[string]map[string]string{"annotations": pod.Annotations}}
	playLoadBytes, _ := json.Marshal(patchData)
	_, err := c.kubeclient.CoreV1().Pods(pod.Namespace).Patch(pod.Name, types.StrategicMergePatchType, playLoadBytes)
	if err != nil {
		klog.Errorf("can't patch pod %v , %v", pod.Name, err)
	}
}

func (c *Controller) updatePodHash(tapp *tappv1.TApp, podId string, pod *corev1.Pod) {
	template, err := getPodTemplate(&tapp.Spec, podId)
	if err != nil {
		klog.Errorf("Failed to get pod template for %s from tapp %s", getPodFullName(pod),
			util.GetTAppFullName(tapp))
		return
	}
	templateHash := c.tappHash.GetTemplateHash(template.Labels)
	uniqHash := c.tappHash.GetUniqHash(template.Labels)
	pod.Labels[hash.TemplateHashKey] = templateHash
	pod.Labels[hash.UniqHashKey] = uniqHash
	patchData := map[string]interface{}{"metadata": map[string]map[string]string{"labels": pod.Labels}}
	playLoadBytes, _ := json.Marshal(patchData)
	_, err = c.kubeclient.CoreV1().Pods(pod.Namespace).Patch(pod.Name, types.StrategicMergePatchType, playLoadBytes)
	if err != nil {
		klog.Errorf("can't patch pod %v , %v", pod.Name, err)
	}

}

func getDesiredInstance(tapp *tappv1.TApp) (running sets.String) {
	completed := sets.NewString()
	for id, status := range tapp.Spec.Statuses {
		// Instance is killed
		if status == tappv1.InstanceKilled {
			completed.Insert(id)
		}
	}

	// If `deletePodAfterAppFinish` is not enabled, pod will be deleted once instance finishes.
	if !getDeletePodAfterAppFinish() || isTAppFinished(tapp) {
		for id, status := range tapp.Status.Statuses {
			// Instance finished
			if status == tappv1.InstanceFailed || status == tappv1.InstanceSucc {
				completed.Insert(id)
			}
		}
	}

	total := sets.NewString()
	for i := 0; i < int(tapp.Spec.Replicas); i++ {
		total.Insert(strconv.Itoa(i))
	}

	running = total.Difference(completed)
	return
}

func makePodMap(pods []*corev1.Pod) map[string]*corev1.Pod {
	podMap := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		index, err := getPodIndex(pod)
		if err != nil {
			klog.Errorf("Failed to get pod index from pod %s: %v", getPodFullName(pod), err)
			continue
		}
		podMap[index] = pod
	}
	return podMap
}

func SetDeletePodAfterAppFinish(value bool) {
	deletePodAfterAppFinish = value
}

func getDeletePodAfterAppFinish() bool {
	return deletePodAfterAppFinish
}
