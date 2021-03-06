package ktail

import (
	"fmt"
	"sync"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type (
	ContainerEnterFunc func(pod *v1.Pod, container *v1.Container) bool
	ContainerExitFunc  func(pod *v1.Pod, container *v1.Container)
	ContainerErrorFunc func(pod *v1.Pod, container *v1.Container, err error)
)

type Callbacks struct {
	OnEvent LogEventFunc
	OnEnter ContainerEnterFunc
	OnExit  ContainerExitFunc
	OnError ContainerErrorFunc
}

type Controller struct {
	sync.Mutex
	clientset     *kubernetes.Clientset
	tailers       map[string]*ContainerTailer
	namespace     string
	labelSelector labels.Selector
	callbacks     Callbacks
}

func NewController(
	clientset *kubernetes.Clientset,
	namespace string,
	labelSelector labels.Selector,
	callbacks Callbacks) *Controller {
	return &Controller{
		clientset:     clientset,
		tailers:       map[string]*ContainerTailer{},
		namespace:     namespace,
		labelSelector: labelSelector,
		callbacks:     callbacks,
	}
}

func (ctl *Controller) Run() {
	podListWatcher := cache.NewListWatchFromClient(
		ctl.clientset.Core().RESTClient(), "pods", ctl.namespace, fields.Everything())

	obj, err := podListWatcher.List(metav1.ListOptions{})
	if err != nil {
		panic(err)
	}
	if podList, ok := obj.(*v1.PodList); ok {
		for _, pod := range podList.Items {
			ctl.onInitialAdd(&pod)
		}
	}

	_, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*v1.Pod); ok {
				ctl.onAdd(pod)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			if pod, ok := obj.(*v1.Pod); ok {
				ctl.onDelete(pod)
			}
		},
	}, cache.Indexers{})

	stopCh := make(chan struct{}, 1)
	go informer.Run(stopCh)
	<-stopCh
}

func (ctl *Controller) onInitialAdd(pod *v1.Pod) {
	if !ctl.labelSelector.Matches(labels.Set(pod.Labels)) {
		return
	}
	for _, container := range pod.Spec.Containers {
		ctl.addContainer(pod, &container, true)
	}
}

func (ctl *Controller) onAdd(pod *v1.Pod) {
	if !ctl.labelSelector.Matches(labels.Set(pod.Labels)) {
		return
	}
	for _, container := range pod.Spec.Containers {
		ctl.addContainer(pod, &container, false)
	}
}

func (ctl *Controller) onDelete(pod *v1.Pod) {
	for _, container := range pod.Spec.Containers {
		ctl.deleteContainer(pod, &container)
	}
}

func (ctl *Controller) addContainer(pod *v1.Pod, container *v1.Container, discoveryPhase bool) {
	ctl.Lock()
	defer ctl.Unlock()

	key := buildKey(pod, container)
	if _, ok := ctl.tailers[key]; ok {
		return
	}

	if !ctl.callbacks.OnEnter(pod, container) {
		return
	}

	targetPod, targetContainer := *pod, *container // Copy to avoid mutation

	tailer := NewContainerTailer(ctl.clientset, targetPod, targetContainer, ctl.callbacks.OnEvent,
		!discoveryPhase)
	go func() {
		if err := tailer.Run(); err != nil {
			ctl.callbacks.OnError(&targetPod, &targetContainer, err)
		}
	}()
	ctl.tailers[key] = tailer
}

func (ctl *Controller) deleteContainer(pod *v1.Pod, container *v1.Container) {
	ctl.Lock()
	defer ctl.Unlock()

	key := buildKey(pod, container)
	if tailer, ok := ctl.tailers[key]; ok {
		delete(ctl.tailers, key)
		tailer.Stop()
		ctl.callbacks.OnExit(pod, container)
	}
}

func buildKey(pod *v1.Pod, container *v1.Container) string {
	return fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)
}
