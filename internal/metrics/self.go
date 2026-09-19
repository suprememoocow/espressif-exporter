package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Self holds the exporter's own metrics, served on /metrics.
//
// Rule: no device label appears anywhere here. The exporter's own cardinality must be
// independent of fleet size, and per-device state is what /probe is for — duplicating it
// would double the storage and create two sources of truth that can disagree.
type Self struct {
	Registry *prometheus.Registry

	DiscoveryEvents   *prometheus.CounterVec
	DiscoveryDropped  prometheus.Counter
	DiscoverySourceUp *prometheus.GaugeVec
	DiscoveryLastSeen *prometheus.GaugeVec

	RegistryDevices   *prometheus.GaugeVec
	RegistryMerges    prometheus.Counter
	RegistryExpiries  *prometheus.CounterVec
	RegistryAddrMoves *prometheus.CounterVec

	AvahiUp         prometheus.Gauge
	AvahiReconnects *prometheus.CounterVec
	AvahiResolvers  prometheus.Gauge

	Probes         *prometheus.CounterVec
	ProbeErrors    *prometheus.CounterVec
	ProbeDuration  *prometheus.HistogramVec
	ProbesInFlight prometheus.Gauge
	ProbesSkipped  *prometheus.CounterVec
	ProbesShared   prometheus.Counter
	DevicesBackoff prometheus.Gauge

	ShellyUnknownComponents *prometheus.CounterVec
	ShellyIdentityCache     prometheus.Gauge
}

// NewSelf builds the exporter's registry. It deliberately does not use
// prometheus.DefaultRegisterer, so nothing a dependency registers globally leaks in.
func NewSelf(buildVersion, commit, goVersion string) *Self {
	reg := prometheus.NewRegistry()
	s := &Self{Registry: reg}

	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: ExporterNamespace, Name: name, Help: help}, labels)
	}
	gaugeVec := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Namespace: ExporterNamespace, Name: name, Help: help}, labels)
	}
	gauge := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(
			prometheus.GaugeOpts{Namespace: ExporterNamespace, Name: name, Help: help})
	}

	s.DiscoveryEvents = counter("discovery_events_total",
		"Discovery observations received.", "source", "type", "kind")
	s.DiscoveryDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ExporterNamespace, Name: "discovery_events_dropped_total",
		Help: "Discovery observations discarded because the registry fell behind."})
	s.DiscoverySourceUp = gaugeVec("discovery_source_up",
		"Whether a discovery backend is currently working.", "source")
	s.DiscoveryLastSeen = gaugeVec("discovery_last_event_timestamp_seconds",
		"When a discovery backend last produced an event. A backend that goes quiet "+
			"without erroring is the hardest failure to notice; alert on this.", "source")

	s.RegistryDevices = gaugeVec("registry_devices", "Known devices.", "kind", "state")
	s.RegistryMerges = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ExporterNamespace, Name: "registry_merges_total",
		Help: "Times two discovery records were found to describe one device."})
	s.RegistryExpiries = counter("registry_expirations_total", "Devices forgotten.", "reason")
	s.RegistryAddrMoves = counter("registry_address_changes_total",
		"Times a device's preferred address changed.", "reason")

	s.AvahiUp = gauge("avahi_up", "Whether the Avahi D-Bus session is established.")
	s.AvahiReconnects = counter("avahi_reconnects_total",
		"Avahi session restarts, by what triggered them.", "reason")
	s.AvahiResolvers = gauge("avahi_active_resolvers",
		"Outstanding Avahi service resolvers. Must stay well below objects-per-client-max.")

	s.Probes = counter("probes_total", "Probes performed.", "kind", "result")
	s.ProbeErrors = counter("probe_errors_total", "Probe failures.", "kind", "reason")
	s.ProbeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ExporterNamespace, Name: "probe_duration_seconds",
		Help:    "Probe duration.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 20},
	}, []string{"kind"})
	s.ProbesInFlight = gauge("probes_in_flight", "Probes currently executing.")
	s.ProbesSkipped = counter("probes_skipped_total",
		"Probes short-circuited without contacting the device.", "reason")
	s.ProbesShared = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ExporterNamespace, Name: "probes_shared_total",
		Help: "Probes served from a concurrent or cached result rather than a device request."})
	s.DevicesBackoff = gauge("devices_in_backoff", "Devices currently being contacted less often.")

	s.ShellyUnknownComponents = counter("shelly_unknown_components_total",
		"Shelly component types seen with no curated extractor. Alert on this increasing: "+
			"it is the signal that a component should be promoted out of the raw namespace.",
		"component")
	s.ShellyIdentityCache = gauge("shelly_identity_cache_entries", "Cached Shelly identities.")

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		s.DiscoveryEvents, s.DiscoveryDropped, s.DiscoverySourceUp, s.DiscoveryLastSeen,
		s.RegistryDevices, s.RegistryMerges, s.RegistryExpiries, s.RegistryAddrMoves,
		s.AvahiUp, s.AvahiReconnects, s.AvahiResolvers,
		s.Probes, s.ProbeErrors, s.ProbeDuration, s.ProbesInFlight, s.ProbesSkipped,
		s.ProbesShared, s.DevicesBackoff,
		s.ShellyUnknownComponents, s.ShellyIdentityCache,
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ExporterNamespace, Name: "build_info",
		Help: "Exporter build metadata. Always 1.",
	}, []string{"version", "commit", "goversion"})
	buildInfo.WithLabelValues(buildVersion, commit, goVersion).Set(1)
	reg.MustRegister(buildInfo)

	return s
}

// ObserveProbe records one probe outcome.
func (s *Self) ObserveProbe(kind, reason string, success bool, d time.Duration) {
	result := "failure"
	if success {
		result = "success"
	}
	s.Probes.WithLabelValues(kind, result).Inc()
	s.ProbeDuration.WithLabelValues(kind).Observe(d.Seconds())
	if !success && reason != "" {
		s.ProbeErrors.WithLabelValues(kind, reason).Inc()
	}
}
