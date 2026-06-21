package controller

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	hermesv1 "github.com/cloudpilot-ai/hermes/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicinformer "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
)

var hermesPolicyGVR = schema.GroupVersionResource{
	Group:    hermesv1.GroupName,
	Version:  "v1alpha1",
	Resource: "hermespolicies",
}

type PolicyBuildTarget struct {
	Platform     string
	PolicyNames  []string
	Acceleration BuildAcceleration
}

type PolicyStatusRecorder interface {
	RecordBuild(ctx context.Context, policyNames []string, imageDigestRef, platform string, phase hermesv1.HermesImagePhase, buildErr error)
}

type HermesPolicyManager struct {
	client     dynamic.Interface
	store      *HermesPolicyStore
	onChangeMu sync.RWMutex
	onChange   func()
}

func NewHermesPolicyManager(cfg *rest.Config) (*HermesPolicyManager, error) {
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &HermesPolicyManager{
		client: client,
		store:  NewHermesPolicyStore(),
	}, nil
}

func (m *HermesPolicyManager) Start(ctx context.Context) error {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(m.client, 30*time.Second, metav1.NamespaceAll, nil)
	informer := factory.ForResource(hermesPolicyGVR).Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if policy, ok := hermesPolicyFromObject(obj); ok {
				m.store.Upsert(policy)
				m.notifyChange()
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPolicy, _ := hermesPolicyFromObject(oldObj)
			if policy, ok := hermesPolicyFromObject(newObj); ok {
				m.store.Upsert(policy)
				if oldPolicy == nil || oldPolicy.Generation != policy.Generation {
					m.notifyChange()
				}
			}
		},
		DeleteFunc: func(obj any) {
			if policy, ok := hermesPolicyFromObject(obj); ok {
				m.store.Delete(policy.Name)
				m.notifyChange()
			}
		},
	})
	if err != nil {
		return err
	}

	runCtx, stop := context.WithCancel(ctx)
	go informer.Run(runCtx.Done())

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		stop()
		return fmt.Errorf("hermespolicy informer cache sync failed")
	}
	log.Printf("hermespolicy watcher synced")
	return nil
}

func (m *HermesPolicyManager) MatchImage(image, defaultPlatform string) []PolicyBuildTarget {
	if m == nil || m.store == nil {
		return nil
	}
	return m.store.MatchImage(image, defaultPlatform)
}

func (m *HermesPolicyManager) SetOnChange(fn func()) {
	m.onChangeMu.Lock()
	defer m.onChangeMu.Unlock()
	m.onChange = fn
}

func (m *HermesPolicyManager) notifyChange() {
	m.onChangeMu.RLock()
	fn := m.onChange
	m.onChangeMu.RUnlock()
	if fn != nil {
		fn()
	}
}

func (m *HermesPolicyManager) RecordBuild(ctx context.Context, policyNames []string, imageDigestRef, platform string, phase hermesv1.HermesImagePhase, buildErr error) {
	if m == nil || m.client == nil || imageDigestRef == "" || platform == "" {
		return
	}
	for _, policyName := range uniqueStrings(policyNames) {
		if err := m.updatePolicyStatus(ctx, policyName, imageDigestRef, platform, phase, buildErr); err != nil {
			log.Printf("update HermesPolicy status failed policy=%s image=%s platform=%s phase=%s: %v", policyName, imageDigestRef, platform, phase, err)
		}
	}
}

func (m *HermesPolicyManager) updatePolicyStatus(ctx context.Context, policyName, imageDigestRef, platform string, phase hermesv1.HermesImagePhase, buildErr error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := m.client.Resource(hermesPolicyGVR).Get(ctx, policyName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var policy hermesv1.HermesPolicy
		if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &policy); err != nil {
			return err
		}

		item := hermesv1.HermesImageStatus{
			ImageDigestRef: imageDigestRef,
			Platform:       platform,
			Phase:          phase,
			LastBuildTime:  metav1.Now(),
		}
		if buildErr != nil {
			item.Error = buildErr.Error()
		}
		setHermesImageStatus(&policy.Status, item)
		policy.Status.ObservedGeneration = policy.Generation

		out, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(&policy)
		if err != nil {
			return err
		}
		_, err = m.client.Resource(hermesPolicyGVR).UpdateStatus(ctx, &unstructured.Unstructured{Object: out}, metav1.UpdateOptions{})
		return err
	})
}

type HermesPolicyStore struct {
	mu       sync.RWMutex
	policies map[string]*hermesv1.HermesPolicy
}

func NewHermesPolicyStore() *HermesPolicyStore {
	return &HermesPolicyStore{
		policies: map[string]*hermesv1.HermesPolicy{},
	}
}

func (s *HermesPolicyStore) Upsert(policy *hermesv1.HermesPolicy) {
	if s == nil || policy == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[policy.Name] = policy.DeepCopy()
}

func (s *HermesPolicyStore) Delete(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policies, name)
}

func (s *HermesPolicyStore) MatchImage(image, defaultPlatform string) []PolicyBuildTarget {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.policies))
	for name := range s.policies {
		names = append(names, name)
	}
	sort.Strings(names)

	platformPolicies := map[string][]string{}
	for _, name := range names {
		policy := s.policies[name]
		if policy == nil || policy.Spec.Paused || !policyMatchesImage(policy, image) {
			continue
		}
		for _, platform := range policyPlatforms(policy, defaultPlatform) {
			platformPolicies[platform] = append(platformPolicies[platform], policy.Name)
		}
	}

	platforms := make([]string, 0, len(platformPolicies))
	for platform := range platformPolicies {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)

	acceleration := buildAccelerationForImageRef(image)
	targets := make([]PolicyBuildTarget, 0, len(platforms))
	for _, platform := range platforms {
		targets = append(targets, PolicyBuildTarget{
			Platform:     platform,
			PolicyNames:  uniqueStrings(platformPolicies[platform]),
			Acceleration: acceleration,
		})
	}
	return targets
}

func hermesPolicyFromObject(obj any) (*hermesv1.HermesPolicy, bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	var policy hermesv1.HermesPolicy
	if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &policy); err != nil {
		log.Printf("decode HermesPolicy failed name=%s: %v", u.GetName(), err)
		return nil, false
	}
	return &policy, true
}

func policyMatchesImage(policy *hermesv1.HermesPolicy, image string) bool {
	if len(policy.Spec.ImageSelectors) == 0 {
		return false
	}
	for _, selector := range policy.Spec.ImageSelectors {
		if selector.ImageRegex == "" {
			continue
		}
		matched, err := regexp.MatchString(selector.ImageRegex, image)
		if err != nil {
			log.Printf("invalid HermesPolicy imageRegex policy=%s regex=%q: %v", policy.Name, selector.ImageRegex, err)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

func policyPlatforms(policy *hermesv1.HermesPolicy, defaultPlatform string) []string {
	values := policy.Spec.Platforms
	if len(values) == 0 && defaultPlatform != "" {
		values = []string{defaultPlatform}
	}
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func setHermesImageStatus(status *hermesv1.HermesPolicyStatus, item hermesv1.HermesImageStatus) {
	for i := range status.Images {
		if status.Images[i].ImageDigestRef == item.ImageDigestRef && status.Images[i].Platform == item.Platform {
			status.Images[i] = item
			updateHermesStatusCounts(status)
			sortHermesImageStatuses(status.Images)
			return
		}
	}
	status.Images = append(status.Images, item)
	updateHermesStatusCounts(status)
	sortHermesImageStatuses(status.Images)
}

func updateHermesStatusCounts(status *hermesv1.HermesPolicyStatus) {
	var ready, failed int32
	for _, image := range status.Images {
		switch image.Phase {
		case hermesv1.HermesImagePhaseReady:
			ready++
		case hermesv1.HermesImagePhaseFailed:
			failed++
		}
	}
	status.Ready = ready
	status.Failed = failed
}

func sortHermesImageStatuses(items []hermesv1.HermesImageStatus) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].ImageDigestRef == items[j].ImageDigestRef {
			return items[i].Platform < items[j].Platform
		}
		return items[i].ImageDigestRef < items[j].ImageDigestRef
	})
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
