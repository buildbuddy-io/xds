package snapshot

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	// AnnotationLocalityPreference on a Service selects how endpoints are
	// split into localities. Values:
	//   "zone"     - split by topology.kubernetes.io/zone
	//   "sub_zone" - split by zone + the sub-zone label
	//   ""         - no split; a single empty-locality group (default)
	AnnotationLocalityPreference = "xds.lmwn.com/locality-preference"

	// LabelZone is the well-known Kubernetes node zone label.
	LabelZone = "topology.kubernetes.io/zone"

	// DefaultSubZoneLabel is the default node label we read as sub-zone when
	// no override is provided. There is no Kubernetes standard for sub-zone,
	// so we default to the common "rack" convention that mirrors the
	// topology.kubernetes.io/zone shape.
	DefaultSubZoneLabel = "topology.kubernetes.io/rack"
)

// LocalityMode describes the locality split for a single service.
type LocalityMode int

const (
	LocalityNone LocalityMode = iota
	LocalityZone
	LocalitySubZone
)

func localityModeFromService(svc *corev1.Service) LocalityMode {
	if svc == nil {
		return LocalityNone
	}
	switch svc.GetAnnotations()[AnnotationLocalityPreference] {
	case "zone":
		return LocalityZone
	case "sub_zone":
		return LocalitySubZone
	default:
		return LocalityNone
	}
}
