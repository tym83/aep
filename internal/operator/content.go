package operator

// Own content charts for aep-cp. Their Chart.yaml carries the
// aep.aenix.io/fixed-version annotation, so builds keep these versions and the
// embedded inventory pins their digests.
const (
	OwnChartRepository         = "oci://ghcr.io/tym83/aep/charts"
	ComponentTypesChart        = "aep-component-types"
	ComponentTypesChartVersion = "0.1.2"
	BuildsChart                = "aep-builds"
	BuildsChartVersion         = "0.1.0"
)
