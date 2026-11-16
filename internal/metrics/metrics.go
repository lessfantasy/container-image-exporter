// Package metrics turns check results into the two exported metric
// families and serves them from an immutable snapshot.
package metrics

import (
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric names.
const (
	InfoName   = "container_image_info"
	UpdateName = "container_image_update_available"
)

// Label names. Every series carries all labels of its family on every
// platform; Docker leaves namespace and workload_kind empty.
var (
	UpdateLabels = []string{"platform", "container", "namespace", "workload_kind", "registry", "repository"}
	InfoLabels   = append(append([]string{}, UpdateLabels...), "current_tag", "latest_tag")
)

// Observation is the check result of one running image.
type Observation struct {
	Platform     string
	Container    string
	Namespace    string
	WorkloadKind string
	Registry     string
	Repository   string
	CurrentTag   string
	// LatestTag is empty when it could not be established.
	LatestTag string
	// OK is false when the check failed or the outcome is unknown.
	OK bool
	// UpdateAvailable is only meaningful when OK is true.
	UpdateAvailable bool
}

// Series is one sample: label values in descriptor order plus a value.
type Series struct {
	Labels []string
	Value  float64
}

// Snapshot is an immutable set of samples.
type Snapshot struct {
	Info   []Series
	Update []Series
}

// Build aggregates observations into unique series.
//
// Several observations can share a label set, because the update metric
// has no tag label and neither metric has a per-container label: a
// workload with two containers from one repository, or a rolling update
// that runs two tags at once. Identical label sets are merged
// conservatively instead of being duplicated or overwritten:
//
//   - container_image_info: one series per distinct label set; the value
//     is 1 only when every merged observation succeeded.
//   - container_image_update_available: one series per label set; it is
//     omitted when any merged observation is unknown, otherwise it is 1
//     when at least one merged observation has an update.
func Build(observations []Observation) *Snapshot {
	type updateAgg struct {
		labels  []string
		unknown bool
		update  bool
	}
	info := map[string]*Series{}
	update := map[string]*updateAgg{}

	for _, o := range observations {
		base := []string{o.Platform, o.Container, o.Namespace, o.WorkloadKind, o.Registry, o.Repository}

		latest := o.LatestTag
		if !o.OK {
			latest = ""
		}
		infoLabels := append(append([]string{}, base...), o.CurrentTag, latest)
		ik := key(infoLabels)
		value := 0.0
		if o.OK {
			value = 1
		}
		if s, ok := info[ik]; ok {
			s.Value = min(s.Value, value)
		} else {
			info[ik] = &Series{Labels: infoLabels, Value: value}
		}

		uk := key(base)
		agg, ok := update[uk]
		if !ok {
			agg = &updateAgg{labels: base}
			update[uk] = agg
		}
		if !o.OK {
			agg.unknown = true
		} else if o.UpdateAvailable {
			agg.update = true
		}
	}

	snap := &Snapshot{}
	for _, s := range info {
		snap.Info = append(snap.Info, *s)
	}
	for _, agg := range update {
		if agg.unknown {
			continue
		}
		value := 0.0
		if agg.update {
			value = 1
		}
		snap.Update = append(snap.Update, Series{Labels: agg.labels, Value: value})
	}
	sortSeries(snap.Info)
	sortSeries(snap.Update)
	return snap
}

func key(labels []string) string { return strings.Join(labels, "\x00") }

func sortSeries(series []Series) {
	sort.Slice(series, func(i, j int) bool { return key(series[i].Labels) < key(series[j].Labels) })
}

// Collector exposes the current snapshot. Collect only reads memory.
type Collector struct {
	source func() *Snapshot
	info   *prometheus.Desc
	update *prometheus.Desc
}

// NewCollector creates a Collector reading snapshots from source. A nil
// snapshot yields no samples.
func NewCollector(source func() *Snapshot) *Collector {
	return &Collector{
		source: source,
		info: prometheus.NewDesc(InfoName,
			"Running container image. 1 = version check succeeded, 0 = check failed or result unknown.",
			InfoLabels, nil),
		update: prometheus.NewDesc(UpdateName,
			"1 = a newer image is available, 0 = up to date. Absent when the update status is unknown.",
			UpdateLabels, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.info
	ch <- c.update
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	snap := c.source()
	if snap == nil {
		return
	}
	for _, s := range snap.Info {
		ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, s.Value, s.Labels...)
	}
	for _, s := range snap.Update {
		ch <- prometheus.MustNewConstMetric(c.update, prometheus.GaugeValue, s.Value, s.Labels...)
	}
}

// NewRegistry returns a dedicated registry holding only the collector: no
// Go runtime, process or promhttp metrics.
func NewRegistry(source func() *Snapshot) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(source))
	return reg
}
