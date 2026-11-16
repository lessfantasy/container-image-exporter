// Package kubernetes discovers running images through the Kubernetes API
// and groups them by their top-level owning workload. Only list (and,
// optionally, get on secrets) requests are made.
package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	kubeauth "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
	"github.com/container-image-exporter/container-image-exporter/internal/image"
)

const (
	listPageSize  = 500
	maxOwnerDepth = 8
	kindPod       = "Pod"
)

// intermediateOwners are the controller kinds that sit between a Pod and
// its top-level workload. Their owner references are loaded on every scan
// so that the chain can be followed upwards:
//
//	Pod -> ReplicaSet -> Deployment
//	Pod -> Job -> CronJob
//
// Any other owner kind (Deployment, StatefulSet, DaemonSet,
// ReplicationController, CronJob, custom resources) terminates the chain
// and becomes the workload. To support a new intermediate kind, add it
// here and grant "list" on it in the ClusterRole.
var intermediateOwners = []struct {
	Kind     string
	Resource schema.GroupVersionResource
}{
	{Kind: "ReplicaSet", Resource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}},
	{Kind: "Job", Resource: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}},
}

// NewClients builds API clients from a kubeconfig path, or from the
// in-cluster ServiceAccount when the path is empty.
func NewClients(kubeconfig string) (kubernetes.Interface, metadata.Interface, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load kubernetes client configuration: %w", err)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	meta, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create kubernetes metadata client: %w", err)
	}
	return core, meta, nil
}

// Discoverer lists running images in all namespaces.
type Discoverer struct {
	core        kubernetes.Interface
	meta        metadata.Interface
	pullSecrets bool
	logger      *slog.Logger

	mu     sync.Mutex
	warned map[string]bool // secrets already reported as unreadable
}

// New creates a Discoverer. With pullSecrets enabled the imagePullSecrets
// of discovered pods are read and used for registry authentication.
func New(core kubernetes.Interface, meta metadata.Interface, pullSecrets bool, logger *slog.Logger) *Discoverer {
	return &Discoverer{core: core, meta: meta, pullSecrets: pullSecrets, logger: logger, warned: map[string]bool{}}
}

type ownerKey struct {
	Group, Kind, Namespace, Name string
}

type workload struct {
	Kind, Name string
}

// Discover implements discovery.Discoverer.
//
// Result: one Instance per (namespace, top-level workload, image
// reference, running digest). Replicas running the same image collapse
// into one Instance; replicas running different images during a rolling
// update stay separate.
func (d *Discoverer) Discover(ctx context.Context) ([]discovery.Instance, error) {
	owners, err := d.loadOwners(ctx)
	if err != nil {
		return nil, err
	}

	secrets := newSecretCache(d)
	type key struct {
		namespace, kind, name, image, digest string
	}
	seen := map[key]bool{}
	var instances []discovery.Instance

	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		pods, err := d.core.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if !isLive(pod) {
				continue
			}
			wl := resolveWorkload(pod, owners)
			var keychain authn.Keychain
			for _, c := range runningImages(pod) {
				k := key{pod.Namespace, wl.Kind, wl.Name, c.image, c.digest}
				if seen[k] {
					continue
				}
				seen[k] = true
				if keychain == nil && d.pullSecrets {
					keychain = secrets.keychain(ctx, pod)
				}
				inst := discovery.Instance{
					Platform:     discovery.PlatformKubernetes,
					Container:    wl.Name,
					Namespace:    pod.Namespace,
					WorkloadKind: wl.Kind,
					RawImage:     c.image,
					Keychain:     keychain,
				}
				ref, err := image.Parse(c.image)
				if err != nil {
					inst.ImageErr = err
				} else {
					inst.Image = ref
					if c.digest != "" {
						inst.Digests = []string{c.digest}
					}
				}
				instances = append(instances, inst)
			}
		}
		if pods.Continue == "" {
			break
		}
		opts.Continue = pods.Continue
	}

	sort.SliceStable(instances, func(i, j int) bool {
		a, b := instances[i], instances[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.WorkloadKind != b.WorkloadKind {
			return a.WorkloadKind < b.WorkloadKind
		}
		if a.Container != b.Container {
			return a.Container < b.Container
		}
		return a.RawImage < b.RawImage
	})
	return instances, nil
}

// loadOwners reads the owner references of all intermediate controllers
// using metadata-only requests.
func (d *Discoverer) loadOwners(ctx context.Context) (map[ownerKey][]metav1.OwnerReference, error) {
	owners := map[ownerKey][]metav1.OwnerReference{}
	for _, kind := range intermediateOwners {
		opts := metav1.ListOptions{Limit: listPageSize}
		for {
			list, err := d.meta.Resource(kind.Resource).List(ctx, opts)
			if err != nil {
				return nil, fmt.Errorf("list %s: %w", kind.Resource.Resource, err)
			}
			for _, item := range list.Items {
				if len(item.OwnerReferences) == 0 {
					continue
				}
				owners[ownerKey{kind.Resource.Group, kind.Kind, item.Namespace, item.Name}] = item.OwnerReferences
			}
			if list.Continue == "" {
				break
			}
			opts.Continue = list.Continue
		}
	}
	return owners, nil
}

// isLive reports whether the pod may have running containers. Succeeded
// and Failed pods (finished Jobs, evicted pods) are not running.
func isLive(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending
}

type runningImage struct {
	image  string // reference from the pod spec
	digest string // registry digest reported by the runtime, may be empty
}

// runningImages returns the images of the containers and init containers
// that the runtime has actually started in this pod. A container counts
// as started when the kubelet reports an image ID for it; containers that
// only exist in the spec (for example in ImagePullBackOff) are skipped.
// Init containers of a live pod are included even after they completed.
func runningImages(pod *corev1.Pod) []runningImage {
	spec := map[string]string{}
	for _, c := range pod.Spec.InitContainers {
		spec[c.Name] = c.Image
	}
	for _, c := range pod.Spec.Containers {
		spec[c.Name] = c.Image
	}

	var out []runningImage
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, st := range statuses {
		if st.ImageID == "" {
			continue
		}
		img := spec[st.Name]
		if img == "" {
			img = st.Image
		}
		out = append(out, runningImage{image: img, digest: image.DigestOf(st.ImageID)})
	}
	return out
}

// resolveWorkload follows controller owner references to the top-level
// workload. Pods without a controller, and static pods owned by a Node,
// are reported as standalone Pods.
func resolveWorkload(pod *corev1.Pod, owners map[ownerKey][]metav1.OwnerReference) workload {
	ref := controllerOf(pod.OwnerReferences)
	if ref == nil || ref.Kind == "Node" {
		return workload{Kind: kindPod, Name: pod.Name}
	}
	wl := workload{Kind: ref.Kind, Name: ref.Name}
	group := groupOf(ref.APIVersion)
	for depth := 0; depth < maxOwnerDepth; depth++ {
		parents, ok := owners[ownerKey{group, wl.Kind, pod.Namespace, wl.Name}]
		if !ok {
			break
		}
		parent := controllerOf(parents)
		if parent == nil {
			break
		}
		wl = workload{Kind: parent.Kind, Name: parent.Name}
		group = groupOf(parent.APIVersion)
	}
	return wl
}

// controllerOf returns the managing controller reference, or the first
// reference when none is marked as controller.
func controllerOf(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

func groupOf(apiVersion string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return "" // core group, e.g. "v1"
	}
	return group
}

// secretCache builds one keychain per distinct imagePullSecrets set
// during a single scan.
type secretCache struct {
	d         *Discoverer
	secrets   map[string]*corev1.Secret // namespace/name, nil when unreadable
	keychains map[string]authn.Keychain
}

func newSecretCache(d *Discoverer) *secretCache {
	return &secretCache{d: d, secrets: map[string]*corev1.Secret{}, keychains: map[string]authn.Keychain{}}
}

func (s *secretCache) keychain(ctx context.Context, pod *corev1.Pod) authn.Keychain {
	if len(pod.Spec.ImagePullSecrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(pod.Spec.ImagePullSecrets))
	for _, ref := range pod.Spec.ImagePullSecrets {
		names = append(names, ref.Name)
	}
	sort.Strings(names)
	setKey := pod.Namespace + "/" + strings.Join(names, ",")
	if kc, ok := s.keychains[setKey]; ok {
		return kc
	}

	var secrets []corev1.Secret
	for _, name := range names {
		if secret := s.secret(ctx, pod.Namespace, name); secret != nil {
			secrets = append(secrets, *secret)
		}
	}
	var kc authn.Keychain
	if len(secrets) > 0 {
		built, err := kubeauth.NewFromPullSecrets(ctx, secrets)
		if err != nil {
			// The error text never contains secret data, only the secret name.
			s.d.warnOnce("keychain:"+setKey, "imagePullSecrets could not be parsed", "namespace", pod.Namespace, "error", err)
		} else {
			kc = built
		}
	}
	s.keychains[setKey] = kc
	return kc
}

func (s *secretCache) secret(ctx context.Context, namespace, name string) *corev1.Secret {
	key := namespace + "/" + name
	if secret, ok := s.secrets[key]; ok {
		return secret
	}
	secret, err := s.d.core.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		s.d.warnOnce("secret:"+key, "imagePullSecret could not be read", "namespace", namespace, "secret", name, "error", err)
		secret = nil
	}
	s.secrets[key] = secret
	return secret
}

func (d *Discoverer) warnOnce(key, msg string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.warned[key] {
		return
	}
	d.warned[key] = true
	d.logger.Warn(msg, args...)
}
