package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func ptr[T any](v T) *T { return &v }

func owner(apiVersion, kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, Controller: ptr(true)}
}

type containerSpec struct {
	name    string
	image   string
	imageID string // empty: not started
	init    bool
}

func ctr(name, image, digest string) containerSpec {
	return containerSpec{name: name, image: image, imageID: "registry.example/" + name + "@" + digest}
}

func pod(namespace, name string, phase corev1.PodPhase, owners []metav1.OwnerReference, containers ...containerSpec) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, OwnerReferences: owners},
		Status:     corev1.PodStatus{Phase: phase},
	}
	for _, c := range containers {
		spec := corev1.Container{Name: c.name, Image: c.image}
		status := corev1.ContainerStatus{Name: c.name, Image: c.image, ImageID: c.imageID}
		if c.init {
			p.Spec.InitContainers = append(p.Spec.InitContainers, spec)
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, status)
		} else {
			p.Spec.Containers = append(p.Spec.Containers, spec)
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, status)
		}
	}
	return p
}

func meta(apiVersion, kind, namespace, name string, owners ...metav1.OwnerReference) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, OwnerReferences: owners},
	}
}

func newDiscoverer(t *testing.T, pullSecrets bool, core []runtime.Object, metas ...runtime.Object) (*Discoverer, *fake.Clientset, *metadatafake.FakeMetadataClient) {
	t.Helper()
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	coreClient := fake.NewClientset(core...)
	metaClient := metadatafake.NewSimpleMetadataClient(scheme, metas...)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(coreClient, metaClient, pullSecrets, logger), coreClient, metaClient
}

// row is the comparable projection of an Instance.
type row struct {
	namespace, kind, name, image, digest string
}

func rows(instances []discovery.Instance) []row {
	out := make([]row, 0, len(instances))
	for _, i := range instances {
		digest := ""
		if len(i.Digests) > 0 {
			digest = i.Digests[0]
		}
		out = append(out, row{i.Namespace, i.WorkloadKind, i.Container, i.Image.String(), digest})
	}
	sort.Slice(out, func(a, b int) bool { return fmt.Sprint(out[a]) < fmt.Sprint(out[b]) })
	return out
}

func TestDiscover(t *testing.T) {
	deployOwner := []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "backend-api-5d9c7b")}
	tests := []struct {
		name  string
		pods  []runtime.Object
		metas []runtime.Object
		want  []row
	}{
		{
			name: "standalone pod",
			pods: []runtime.Object{
				pod("default", "debug", corev1.PodRunning, nil, ctr("shell", "busybox:1.36.1", digestA)),
			},
			want: []row{{"default", "Pod", "debug", "docker.io/library/busybox:1.36.1", digestA}},
		},
		{
			name: "deployment with multiple replicas is reported once",
			pods: []runtime.Object{
				pod("prod", "backend-api-5d9c7b-aaaaa", corev1.PodRunning, deployOwner, ctr("api", "ghcr.io/org/api:1.4.2", digestA)),
				pod("prod", "backend-api-5d9c7b-bbbbb", corev1.PodRunning, deployOwner, ctr("api", "ghcr.io/org/api:1.4.2", digestA)),
				pod("prod", "backend-api-5d9c7b-ccccc", corev1.PodRunning, deployOwner, ctr("api", "ghcr.io/org/api:1.4.2", digestA)),
			},
			metas: []runtime.Object{
				meta("apps/v1", "ReplicaSet", "prod", "backend-api-5d9c7b", owner("apps/v1", "Deployment", "backend-api")),
			},
			want: []row{{"prod", "Deployment", "backend-api", "ghcr.io/org/api:1.4.2", digestA}},
		},
		{
			name: "replicaset owned by deployment creates no series of its own",
			pods: []runtime.Object{
				pod("prod", "web-7f-x", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "web-7f")}, ctr("web", "nginx:1.26.2", digestA)),
			},
			metas: []runtime.Object{
				meta("apps/v1", "ReplicaSet", "prod", "web-7f", owner("apps/v1", "Deployment", "web")),
			},
			want: []row{{"prod", "Deployment", "web", "docker.io/library/nginx:1.26.2", digestA}},
		},
		{
			name: "bare replicaset",
			pods: []runtime.Object{
				pod("prod", "rs-x", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "rs")}, ctr("web", "nginx:1.26.2", digestA)),
			},
			metas: []runtime.Object{meta("apps/v1", "ReplicaSet", "prod", "rs")},
			want:  []row{{"prod", "ReplicaSet", "rs", "docker.io/library/nginx:1.26.2", digestA}},
		},
		{
			name: "statefulset",
			pods: []runtime.Object{
				pod("db", "pg-0", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "StatefulSet", "pg")}, ctr("pg", "postgres:16.4", digestA)),
				pod("db", "pg-1", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "StatefulSet", "pg")}, ctr("pg", "postgres:16.4", digestA)),
			},
			want: []row{{"db", "StatefulSet", "pg", "docker.io/library/postgres:16.4", digestA}},
		},
		{
			name: "daemonset",
			pods: []runtime.Object{
				pod("kube-system", "proxy-a", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "DaemonSet", "kube-proxy")}, ctr("p", "registry.k8s.io/kube-proxy:v1.31.2", digestA)),
				pod("kube-system", "proxy-b", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "DaemonSet", "kube-proxy")}, ctr("p", "registry.k8s.io/kube-proxy:v1.31.2", digestA)),
			},
			want: []row{{"kube-system", "DaemonSet", "kube-proxy", "registry.k8s.io/kube-proxy:v1.31.2", digestA}},
		},
		{
			name: "replicationcontroller",
			pods: []runtime.Object{
				pod("legacy", "rc-x", corev1.PodRunning, []metav1.OwnerReference{owner("v1", "ReplicationController", "rc")}, ctr("a", "quay.io/org/app:2.0.0", digestA)),
			},
			want: []row{{"legacy", "ReplicationController", "rc", "quay.io/org/app:2.0.0", digestA}},
		},
		{
			name: "job owned by cronjob",
			pods: []runtime.Object{
				pod("batch", "backup-28840-abcde", corev1.PodRunning, []metav1.OwnerReference{owner("batch/v1", "Job", "backup-28840")}, ctr("b", "ghcr.io/org/backup:3.1.0", digestA)),
			},
			metas: []runtime.Object{
				meta("batch/v1", "Job", "batch", "backup-28840", owner("batch/v1", "CronJob", "backup")),
			},
			want: []row{{"batch", "CronJob", "backup", "ghcr.io/org/backup:3.1.0", digestA}},
		},
		{
			name: "standalone job is counted once, not as job and pod",
			pods: []runtime.Object{
				pod("batch", "migrate-abcde", corev1.PodRunning, []metav1.OwnerReference{owner("batch/v1", "Job", "migrate")}, ctr("m", "ghcr.io/org/migrate:1.0.0", digestA)),
			},
			metas: []runtime.Object{meta("batch/v1", "Job", "batch", "migrate")},
			want:  []row{{"batch", "Job", "migrate", "ghcr.io/org/migrate:1.0.0", digestA}},
		},
		{
			name: "multiple containers per pod",
			pods: []runtime.Object{
				pod("prod", "app-0", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "StatefulSet", "app")},
					ctr("app", "ghcr.io/org/app:1.0.0", digestA),
					ctr("sidecar", "envoyproxy/envoy:v1.31.0", digestB)),
			},
			want: []row{
				{"prod", "StatefulSet", "app", "docker.io/envoyproxy/envoy:v1.31.0", digestB},
				{"prod", "StatefulSet", "app", "ghcr.io/org/app:1.0.0", digestA},
			},
		},
		{
			name: "init containers",
			pods: []runtime.Object{
				pod("prod", "app-0", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "StatefulSet", "app")},
					containerSpec{name: "init", image: "busybox:1.36.1", imageID: "docker.io/library/busybox@" + digestB, init: true},
					ctr("app", "ghcr.io/org/app:1.0.0", digestA)),
			},
			want: []row{
				{"prod", "StatefulSet", "app", "docker.io/library/busybox:1.36.1", digestB},
				{"prod", "StatefulSet", "app", "ghcr.io/org/app:1.0.0", digestA},
			},
		},
		{
			name: "rolling update keeps both running versions",
			pods: []runtime.Object{
				pod("prod", "api-old-1", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "api-old")}, ctr("api", "ghcr.io/org/api:1.4.2", digestA)),
				pod("prod", "api-old-2", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "api-old")}, ctr("api", "ghcr.io/org/api:1.4.2", digestA)),
				pod("prod", "api-new-1", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "api-new")}, ctr("api", "ghcr.io/org/api:1.5.0", digestB)),
				// Scheduled, image not pulled yet: desired, not running.
				pod("prod", "api-new-2", corev1.PodPending, []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "api-new")}, containerSpec{name: "api", image: "ghcr.io/org/api:1.5.0"}),
			},
			metas: []runtime.Object{
				meta("apps/v1", "ReplicaSet", "prod", "api-old", owner("apps/v1", "Deployment", "api")),
				meta("apps/v1", "ReplicaSet", "prod", "api-new", owner("apps/v1", "Deployment", "api")),
			},
			want: []row{
				{"prod", "Deployment", "api", "ghcr.io/org/api:1.4.2", digestA},
				{"prod", "Deployment", "api", "ghcr.io/org/api:1.5.0", digestB},
			},
		},
		{
			name: "replicas on one floating tag with different digests stay distinct",
			pods: []runtime.Object{
				pod("prod", "web-1", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "DaemonSet", "web")}, ctr("web", "nginx:latest", digestA)),
				pod("prod", "web-2", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "DaemonSet", "web")}, ctr("web", "nginx:latest", digestB)),
			},
			want: []row{
				{"prod", "DaemonSet", "web", "docker.io/library/nginx:latest", digestA},
				{"prod", "DaemonSet", "web", "docker.io/library/nginx:latest", digestB},
			},
		},
		{
			name: "pods that are not running",
			pods: []runtime.Object{
				pod("batch", "done", corev1.PodSucceeded, []metav1.OwnerReference{owner("batch/v1", "Job", "done")}, ctr("j", "ghcr.io/org/job:1.0.0", digestA)),
				pod("batch", "crashed", corev1.PodFailed, nil, ctr("j", "ghcr.io/org/job:1.0.0", digestA)),
				pod("batch", "pending", corev1.PodPending, nil, containerSpec{name: "j", image: "ghcr.io/org/job:2.0.0"}),
				pod("batch", "pull-backoff", corev1.PodRunning, nil,
					ctr("ok", "ghcr.io/org/ok:1.0.0", digestA),
					containerSpec{name: "broken", image: "ghcr.io/org/missing:9.9.9"}),
			},
			want: []row{{"batch", "Pod", "pull-backoff", "ghcr.io/org/ok:1.0.0", digestA}},
		},
		{
			name: "static pod owned by a node is a standalone pod",
			pods: []runtime.Object{
				pod("kube-system", "etcd-node1", corev1.PodRunning, []metav1.OwnerReference{owner("v1", "Node", "node1")}, ctr("etcd", "registry.k8s.io/etcd:3.5.15-0", digestA)),
			},
			want: []row{{"kube-system", "Pod", "etcd-node1", "registry.k8s.io/etcd:3.5.15-0", digestA}},
		},
		{
			name: "custom controller kind is kept as the workload",
			pods: []runtime.Object{
				pod("prod", "roll-abc-1", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", "roll-abc")}, ctr("a", "ghcr.io/org/app:1.0.0", digestA)),
			},
			metas: []runtime.Object{
				meta("apps/v1", "ReplicaSet", "prod", "roll-abc", owner("argoproj.io/v1alpha1", "Rollout", "roll")),
			},
			want: []row{{"prod", "Rollout", "roll", "ghcr.io/org/app:1.0.0", digestA}},
		},
		{
			name: "same workload name in two namespaces",
			pods: []runtime.Object{
				pod("a", "web-1", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "StatefulSet", "web")}, ctr("w", "nginx:1.26.2", digestA)),
				pod("b", "web-1", corev1.PodRunning, []metav1.OwnerReference{owner("apps/v1", "StatefulSet", "web")}, ctr("w", "nginx:1.26.2", digestA)),
			},
			want: []row{
				{"a", "StatefulSet", "web", "docker.io/library/nginx:1.26.2", digestA},
				{"b", "StatefulSet", "web", "docker.io/library/nginx:1.26.2", digestA},
			},
		},
		{
			name: "runtime image id without a registry digest",
			pods: []runtime.Object{
				pod("default", "local", corev1.PodRunning, nil, containerSpec{name: "c", image: "nginx:latest", imageID: "docker://" + digestA}),
			},
			want: []row{{"default", "Pod", "local", "docker.io/library/nginx:latest", ""}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _, _ := newDiscoverer(t, false, tt.pods, tt.metas...)
			got, err := d.Discover(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			gotRows := rows(got)
			if fmt.Sprint(gotRows) != fmt.Sprint(tt.want) {
				t.Errorf("got\n  %v\nwant\n  %v", gotRows, tt.want)
			}
			for _, inst := range got {
				if inst.Platform != discovery.PlatformKubernetes {
					t.Errorf("platform = %q", inst.Platform)
				}
			}
		})
	}
}

func TestDiscoverIsDeterministic(t *testing.T) {
	objs := []runtime.Object{
		pod("b", "p2", corev1.PodRunning, nil, ctr("c", "nginx:1.26.2", digestA)),
		pod("a", "p1", corev1.PodRunning, nil, ctr("c", "redis:7.4.1", digestA), ctr("d", "nginx:1.26.2", digestB)),
	}
	d, _, _ := newDiscoverer(t, false, objs)
	first, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, _ := d.Discover(context.Background())
	if fmt.Sprint(rows(first)) != fmt.Sprint(rows(second)) || first[0].Namespace != "a" {
		t.Errorf("unstable order: %v vs %v", first, second)
	}
}

func TestUnparsableImage(t *testing.T) {
	d, _, _ := newDiscoverer(t, false, []runtime.Object{
		pod("default", "weird", corev1.PodRunning, nil, ctr("c", "Not A Valid Image", digestA)),
	})
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ImageErr == nil {
		t.Errorf("got %+v, want one instance with ImageErr", got)
	}
}

func TestAPIFailuresAreErrors(t *testing.T) {
	boom := errors.New("apiserver unavailable")

	// Pods cannot be listed.
	d, core, _ := newDiscoverer(t, false, nil)
	core.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
	if got, err := d.Discover(context.Background()); err == nil || got != nil {
		t.Errorf("pods: got %v, %v", got, err)
	}

	// Owners cannot be listed: partial discovery must not yield a
	// misleading inventory (pods would be attributed to ReplicaSets).
	d, _, metaClient := newDiscoverer(t, false, []runtime.Object{
		pod("default", "p", corev1.PodRunning, nil, ctr("c", "nginx:1.26.2", digestA)),
	})
	metaClient.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
	if got, err := d.Discover(context.Background()); err == nil || got != nil {
		t.Errorf("replicasets: got %v, %v", got, err)
	}
}

func TestReadOnlyRequests(t *testing.T) {
	d, core, metaClient := newDiscoverer(t, true, []runtime.Object{
		pod("default", "p", corev1.PodRunning, nil, ctr("c", "nginx:1.26.2", digestA)),
	})
	if _, err := d.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, a := range append(core.Actions(), metaClient.Actions()...) {
		if verb := a.GetVerb(); verb != "list" && verb != "get" {
			t.Errorf("non read-only request: %s %s", verb, a.GetResource().Resource)
		}
	}
}

func pullSecret(namespace, name, registry, user, pass string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(fmt.Sprintf(`{"auths":{%q:{"username":%q,"password":%q}}}`, registry, user, pass)),
		},
	}
}

func TestImagePullSecrets(t *testing.T) {
	private := pod("prod", "api-1", corev1.PodRunning, nil, ctr("api", "registry.example.com/team/api:1.0.0", digestA))
	private.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}, {Name: "missing"}}
	public := pod("prod", "web-1", corev1.PodRunning, nil, ctr("web", "nginx:1.26.2", digestA))
	objs := []runtime.Object{private, public, pullSecret("prod", "regcred", "registry.example.com", "robot", "s3cret")}

	target, _ := name.NewRegistry("registry.example.com")

	t.Run("enabled", func(t *testing.T) {
		d, core, _ := newDiscoverer(t, true, objs)
		got, err := d.Discover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var withKeychain, without int
		for _, inst := range got {
			if inst.Keychain == nil {
				without++
				continue
			}
			withKeychain++
			auth, err := inst.Keychain.Resolve(target)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := auth.Authorization()
			if err != nil || cfg.Username != "robot" || cfg.Password != "s3cret" {
				t.Errorf("credentials = %+v, %v", cfg, err)
			}
			other, _ := name.NewRegistry("ghcr.io")
			if auth, _ := inst.Keychain.Resolve(other); auth != authn.Anonymous {
				t.Error("secret used for a registry it does not cover")
			}
		}
		if withKeychain != 1 || without != 1 {
			t.Errorf("keychains: with=%d without=%d", withKeychain, without)
		}

		// An unreadable secret is tolerated and only reported once.
		if _, err := d.Discover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !d.warned["secret:prod/missing"] {
			t.Error("unreadable secret not recorded")
		}
		gets := 0
		for _, a := range core.Actions() {
			if a.GetVerb() == "get" && a.GetResource().Resource == "secrets" {
				gets++
			}
		}
		if gets != 4 { // two secrets, two scans
			t.Errorf("secret gets = %d, want 4", gets)
		}
	})

	t.Run("disabled by default", func(t *testing.T) {
		d, core, _ := newDiscoverer(t, false, objs)
		got, err := d.Discover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, inst := range got {
			if inst.Keychain != nil {
				t.Error("keychain built although the feature is disabled")
			}
		}
		for _, a := range core.Actions() {
			if a.GetResource().Resource == "secrets" {
				t.Error("secrets accessed although the feature is disabled")
			}
		}
	})
}
