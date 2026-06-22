package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func StartPodWatcher(ctx context.Context, cfg Config, builder *Builder) error {
	kcfg, err := kubeConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(kcfg)
	if err != nil {
		return err
	}
	policyManager, err := NewHermesPolicyManager(kcfg)
	if err != nil {
		return err
	}
	if err := policyManager.Start(ctx); err != nil {
		return err
	}
	builder.SetPolicyStatusRecorder(policyManager)

	var options []kubeinformers.SharedInformerOption
	if cfg.WatchNamespace != "" {
		options = append(options, kubeinformers.WithNamespace(cfg.WatchNamespace))
	}
	options = append(options, kubeinformers.WithTweakListOptions(func(opts *metav1.ListOptions) {}))

	factory := kubeinformers.NewSharedInformerFactoryWithOptions(client, 30*time.Second, options...)
	informer := factory.Core().V1().Pods().Informer()
	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod, ok := obj.(*corev1.Pod)
			if ok {
				enqueuePodImages(ctx, client, builder, policyManager, pod, cfg.Platform, "pod-add")
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, oldOK := oldObj.(*corev1.Pod)
			pod, ok := newObj.(*corev1.Pod)
			if ok {
				if oldOK && !podImageInputsChanged(oldPod, pod) {
					return
				}
				enqueuePodImages(ctx, client, builder, policyManager, pod, cfg.Platform, "pod-update")
			}
		},
	})
	if err != nil {
		return err
	}
	policyManager.SetOnChange(func() {
		enqueuePodsFromStore(ctx, client, builder, policyManager, informer.GetStore(), cfg.Platform, "policy-update")
	})

	log.Printf("starting pod watcher namespace=%q kubeconfig=%q", cfg.WatchNamespace, cfg.Kubeconfig)
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("pod informer cache sync failed")
	}
	enqueuePodsFromStore(ctx, client, builder, policyManager, informer.GetStore(), cfg.Platform, "pod-sync")
	log.Printf("pod watcher synced")
	<-ctx.Done()
	return nil
}

func enqueuePodImages(ctx context.Context, client kubernetes.Interface, builder *Builder, policies *HermesPolicyManager, pod *corev1.Pod, platform, reason string) {
	auths, err := registryAuthsFromPod(ctx, client, pod)
	if err != nil {
		log.Printf("load imagePullSecrets failed namespace=%s pod=%s: %v", pod.Namespace, pod.Name, err)
	}
	for _, c := range pod.Spec.InitContainers {
		enqueueImage(builder, policies, c.Image, platform, reason+"/"+pod.Namespace+"/"+pod.Name, auths)
	}
	for _, c := range pod.Spec.Containers {
		enqueueImage(builder, policies, c.Image, platform, reason+"/"+pod.Namespace+"/"+pod.Name, auths)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		enqueueImage(builder, policies, c.Image, platform, reason+"/"+pod.Namespace+"/"+pod.Name, auths)
	}
}

func enqueueImage(builder *Builder, policies *HermesPolicyManager, image, defaultPlatform, reason string, auths []RegistryAuth) {
	if policies == nil {
		return
	}
	for _, target := range policies.MatchImage(image, defaultPlatform) {
		builder.Enqueue(BuildTask{
			SourceImageRef: image,
			Platform:       target.Platform,
			Reason:         reason,
			PolicyNames:    target.PolicyNames,
			RegistryAuths:  auths,
			Acceleration:   target.Acceleration,
		})
	}
}

func podImageInputsChanged(oldPod, newPod *corev1.Pod) bool {
	oldInputs := podImageInputs(oldPod)
	newInputs := podImageInputs(newPod)
	if len(oldInputs) != len(newInputs) {
		return true
	}
	for i := range oldInputs {
		if oldInputs[i] != newInputs[i] {
			return true
		}
	}
	return false
}

func podImageInputs(pod *corev1.Pod) []string {
	if pod == nil {
		return nil
	}
	inputs := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers)+len(pod.Spec.EphemeralContainers)+len(pod.Spec.ImagePullSecrets))
	for _, c := range pod.Spec.InitContainers {
		inputs = append(inputs, "init:"+c.Name+"="+c.Image)
	}
	for _, c := range pod.Spec.Containers {
		inputs = append(inputs, "container:"+c.Name+"="+c.Image)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		inputs = append(inputs, "ephemeral:"+c.Name+"="+c.Image)
	}
	for _, s := range pod.Spec.ImagePullSecrets {
		inputs = append(inputs, "secret:"+s.Name)
	}
	return inputs
}

func enqueuePodsFromStore(ctx context.Context, client kubernetes.Interface, builder *Builder, policies *HermesPolicyManager, store cache.Store, platform, reason string) {
	for _, obj := range store.List() {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		enqueuePodImages(ctx, client, builder, policies, pod, platform, reason)
	}
}

func kubeConfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".kube", "config")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return clientcmd.BuildConfigFromFlags("", candidate)
		}
	}
	return nil, fmt.Errorf("no kubeconfig found and in-cluster config is unavailable")
}
