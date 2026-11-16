package metrics

import (
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func dockerObs(container, repo, current, latest string, ok, update bool) Observation {
	return Observation{
		Platform: "docker", Container: container, Registry: "docker.io", Repository: repo,
		CurrentTag: current, LatestTag: latest, OK: ok, UpdateAvailable: update,
	}
}

func kubeObs(ns, kind, name, repo, current, latest string, ok, update bool) Observation {
	return Observation{
		Platform: "kubernetes", Container: name, Namespace: ns, WorkloadKind: kind,
		Registry: "ghcr.io", Repository: repo,
		CurrentTag: current, LatestTag: latest, OK: ok, UpdateAvailable: update,
	}
}

func source(obs ...Observation) func() *Snapshot {
	snap := Build(obs)
	return func() *Snapshot { return snap }
}

func TestLabelNames(t *testing.T) {
	wantUpdate := []string{"platform", "container", "namespace", "workload_kind", "registry", "repository"}
	wantInfo := append(append([]string{}, wantUpdate...), "current_tag", "latest_tag")
	if !reflect.DeepEqual(UpdateLabels, wantUpdate) {
		t.Errorf("UpdateLabels = %v", UpdateLabels)
	}
	if !reflect.DeepEqual(InfoLabels, wantInfo) {
		t.Errorf("InfoLabels = %v", InfoLabels)
	}
}

func TestExposition(t *testing.T) {
	reg := NewRegistry(source(
		dockerObs("web", "library/nginx", "1.26.2", "1.27.3", true, true),
		dockerObs("cache", "library/redis", "latest", "latest", true, false),
		kubeObs("prod", "Deployment", "backend-api", "org/api", "v1.4.2", "v1.4.2", true, false),
	))
	want := `
# HELP container_image_info Running container image. 1 = version check succeeded, 0 = check failed or result unknown.
# TYPE container_image_info gauge
container_image_info{container="backend-api",current_tag="v1.4.2",latest_tag="v1.4.2",namespace="prod",platform="kubernetes",registry="ghcr.io",repository="org/api",workload_kind="Deployment"} 1
container_image_info{container="cache",current_tag="latest",latest_tag="latest",namespace="",platform="docker",registry="docker.io",repository="library/redis",workload_kind=""} 1
container_image_info{container="web",current_tag="1.26.2",latest_tag="1.27.3",namespace="",platform="docker",registry="docker.io",repository="library/nginx",workload_kind=""} 1
# HELP container_image_update_available 1 = a newer image is available, 0 = up to date. Absent when the update status is unknown.
# TYPE container_image_update_available gauge
container_image_update_available{container="backend-api",namespace="prod",platform="kubernetes",registry="ghcr.io",repository="org/api",workload_kind="Deployment"} 0
container_image_update_available{container="cache",namespace="",platform="docker",registry="docker.io",repository="library/redis",workload_kind=""} 0
container_image_update_available{container="web",namespace="",platform="docker",registry="docker.io",repository="library/nginx",workload_kind=""} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}

// TestOnlyTwoFamilies verifies that nothing but the two application
// metrics is exposed: no go_*, process_*, promhttp_* or prometheus_*.
func TestOnlyTwoFamilies(t *testing.T) {
	reg := NewRegistry(source(dockerObs("web", "library/nginx", "1.26.2", "1.27.3", true, true)))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range families {
		names = append(names, f.GetName())
		for _, prefix := range []string{"go_", "process_", "promhttp_", "prometheus_"} {
			if strings.HasPrefix(f.GetName(), prefix) {
				t.Errorf("default metric exposed: %s", f.GetName())
			}
		}
	}
	if want := []string{InfoName, UpdateName}; !reflect.DeepEqual(names, want) {
		t.Errorf("families = %v, want %v", names, want)
	}
}

// TestLabelConsistency verifies that Docker and Kubernetes series carry
// exactly the same label names.
func TestLabelConsistency(t *testing.T) {
	reg := NewRegistry(source(
		dockerObs("web", "library/nginx", "1.26.2", "1.27.3", true, true),
		kubeObs("prod", "Deployment", "api", "org/api", "v1.4.2", "v1.5.0", true, true),
	))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		InfoName:   {"container", "current_tag", "latest_tag", "namespace", "platform", "registry", "repository", "workload_kind"},
		UpdateName: {"container", "namespace", "platform", "registry", "repository", "workload_kind"},
	}
	for _, f := range families {
		if len(f.GetMetric()) != 2 {
			t.Errorf("%s: %d series, want 2", f.GetName(), len(f.GetMetric()))
		}
		for _, m := range f.GetMetric() {
			var names []string
			values := map[string]string{}
			for _, l := range m.GetLabel() {
				names = append(names, l.GetName())
				values[l.GetName()] = l.GetValue()
			}
			if !reflect.DeepEqual(names, want[f.GetName()]) {
				t.Errorf("%s labels = %v, want %v", f.GetName(), names, want[f.GetName()])
			}
			if values["platform"] == "docker" && (values["namespace"] != "" || values["workload_kind"] != "") {
				t.Errorf("docker series has kubernetes label values: %v", values)
			}
		}
	}
}

func TestFailedCheck(t *testing.T) {
	// A stale LatestTag on a failed observation must not be published.
	snap := Build([]Observation{dockerObs("web", "library/nginx", "1.26.2", "1.27.3", false, true)})
	if len(snap.Info) != 1 || snap.Info[0].Value != 0 {
		t.Fatalf("info = %+v, want one series with value 0", snap.Info)
	}
	if latest := snap.Info[0].Labels[7]; latest != "" {
		t.Errorf("latest_tag = %q, want empty", latest)
	}
	if len(snap.Update) != 0 {
		t.Errorf("update series published for an unknown status: %+v", snap.Update)
	}
}

func TestBuildAggregation(t *testing.T) {
	tests := []struct {
		name       string
		obs        []Observation
		wantInfo   map[string]float64 // "current|latest" -> value
		wantUpdate []float64          // values of the update series; nil = omitted
	}{
		{
			name:       "successful check without update",
			obs:        []Observation{kubeObs("p", "Deployment", "api", "org/api", "1.0.0", "1.0.0", true, false)},
			wantInfo:   map[string]float64{"1.0.0|1.0.0": 1},
			wantUpdate: []float64{0},
		},
		{
			name:       "successful check with update",
			obs:        []Observation{kubeObs("p", "Deployment", "api", "org/api", "1.0.0", "1.1.0", true, true)},
			wantInfo:   map[string]float64{"1.0.0|1.1.0": 1},
			wantUpdate: []float64{1},
		},
		{
			name:       "unknown update status",
			obs:        []Observation{kubeObs("p", "Deployment", "api", "org/api", "abc123", "", false, false)},
			wantInfo:   map[string]float64{"abc123|": 0},
			wantUpdate: nil,
		},
		{
			name: "rolling update: both tags visible, one update series",
			obs: []Observation{
				kubeObs("p", "Deployment", "api", "org/api", "1.0.0", "1.1.0", true, true),
				kubeObs("p", "Deployment", "api", "org/api", "1.1.0", "1.1.0", true, false),
			},
			wantInfo:   map[string]float64{"1.0.0|1.1.0": 1, "1.1.0|1.1.0": 1},
			wantUpdate: []float64{1},
		},
		{
			name: "two containers, same repository and tag: merged, not duplicated",
			obs: []Observation{
				kubeObs("p", "Deployment", "api", "org/api", "1.0.0", "1.0.0", true, false),
				kubeObs("p", "Deployment", "api", "org/api", "1.0.0", "1.0.0", true, false),
			},
			wantInfo:   map[string]float64{"1.0.0|1.0.0": 1},
			wantUpdate: []float64{0},
		},
		{
			name: "same floating tag, one replica outdated",
			obs: []Observation{
				kubeObs("p", "DaemonSet", "web", "org/web", "latest", "latest", true, false),
				kubeObs("p", "DaemonSet", "web", "org/web", "latest", "latest", true, true),
			},
			wantInfo:   map[string]float64{"latest|latest": 1},
			wantUpdate: []float64{1},
		},
		{
			name: "one member unknown hides the update series",
			obs: []Observation{
				kubeObs("p", "Deployment", "api", "org/api", "1.0.0", "1.0.0", true, false),
				kubeObs("p", "Deployment", "api", "org/api", "nightly-x", "", false, false),
			},
			wantInfo:   map[string]float64{"1.0.0|1.0.0": 1, "nightly-x|": 0},
			wantUpdate: nil,
		},
		{
			name: "identical info labels with mixed success report failure",
			obs: []Observation{
				kubeObs("p", "Deployment", "api", "org/api", "latest", "", false, false),
				kubeObs("p", "Deployment", "api", "org/api", "latest", "", false, false),
			},
			wantInfo:   map[string]float64{"latest|": 0},
			wantUpdate: nil,
		},
		{
			name:       "no observations",
			obs:        nil,
			wantInfo:   map[string]float64{},
			wantUpdate: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := Build(tt.obs)
			gotInfo := map[string]float64{}
			for _, s := range snap.Info {
				gotInfo[s.Labels[6]+"|"+s.Labels[7]] = s.Value
			}
			if !reflect.DeepEqual(gotInfo, tt.wantInfo) {
				t.Errorf("info = %v, want %v", gotInfo, tt.wantInfo)
			}
			var gotUpdate []float64
			for _, s := range snap.Update {
				gotUpdate = append(gotUpdate, s.Value)
			}
			if !reflect.DeepEqual(gotUpdate, tt.wantUpdate) {
				t.Errorf("update = %v, want %v", gotUpdate, tt.wantUpdate)
			}

			// Whatever the input, gathering must never fail on duplicates.
			if _, err := NewRegistry(func() *Snapshot { return snap }).Gather(); err != nil {
				t.Errorf("Gather: %v", err)
			}
		})
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a := []Observation{
		dockerObs("b", "library/nginx", "1.0.0", "1.0.0", true, false),
		dockerObs("a", "library/redis", "7.0.0", "7.2.0", true, true),
		dockerObs("c", "library/x", "bad", "", false, false),
	}
	b := []Observation{a[2], a[0], a[1]}
	if !reflect.DeepEqual(Build(a), Build(b)) {
		t.Error("snapshot depends on observation order")
	}
}

func TestNilSnapshotYieldsNoSamples(t *testing.T) {
	reg := NewRegistry(func() *Snapshot { return nil })
	families, err := reg.Gather()
	if err != nil || len(families) != 0 {
		t.Errorf("families = %v, err = %v", families, err)
	}
}

func TestLint(t *testing.T) {
	reg := NewRegistry(source(dockerObs("web", "library/nginx", "1.26.2", "1.27.3", true, true)))
	problems, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}
