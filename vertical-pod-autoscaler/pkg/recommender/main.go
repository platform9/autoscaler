/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/common"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input/history"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/routines"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/metrics"
	metrics_quality "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/metrics/quality"
	metrics_recommender "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/metrics/recommender"
	kube_flag "k8s.io/component-base/cli/flag"
	klog "k8s.io/klog/v2"
)

var (
	recommenderName        = flag.String("recommender-name", input.DefaultRecommenderName, "Set the recommender name. Recommender will generate recommendations for VPAs that configure the same recommender name. If the recommender name is left as default it will also generate recommendations that don't explicitly specify recommender. You shouldn't run two recommenders with the same name in a cluster.")
	metricsFetcherInterval = flag.Duration("recommender-interval", 1*time.Minute, `How often metrics should be fetched`)
	checkpointsGCInterval  = flag.Duration("checkpoints-gc-interval", 10*time.Minute, `How often orphaned checkpoints should be garbage collected`)
	prometheusAddress      = flag.String("prometheus-address", "", `Where to reach for Prometheus metrics`)
	prometheusJobName      = flag.String("prometheus-cadvisor-job-name", "kubernetes-cadvisor", `Name of the prometheus job name which scrapes the cAdvisor metrics`)
	address                = flag.String("address", ":8942", "The address to expose Prometheus metrics.")
	kubeconfig             = flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	kubeApiQps             = flag.Float64("kube-api-qps", 5.0, `QPS limit when making requests to Kubernetes apiserver`)
	kubeApiBurst           = flag.Float64("kube-api-burst", 10.0, `QPS burst limit when making requests to Kubernetes apiserver`)

	storage = flag.String("storage", "", `Specifies storage mode. Supported values: prometheus, checkpoint (default)`)
	// prometheus history provider configs
	historyLength       = flag.String("history-length", "8d", `How much time back prometheus have to be queried to get historical metrics`)
	historyResolution   = flag.String("history-resolution", "1h", `Resolution at which Prometheus is queried for historical metrics`)
	queryTimeout        = flag.String("prometheus-query-timeout", "5m", `How long to wait before killing long queries`)
	podLabelPrefix      = flag.String("pod-label-prefix", "pod_label_", `Which prefix to look for pod labels in metrics`)
	podLabelsMetricName = flag.String("metric-for-pod-labels", "up{job=\"kubernetes-pods\"}", `Which metric to look for pod labels in metrics`)
	podNamespaceLabel   = flag.String("pod-namespace-label", "kubernetes_namespace", `Label name to look for pod namespaces`)
	podNameLabel        = flag.String("pod-name-label", "kubernetes_pod_name", `Label name to look for pod names`)
	ctrNamespaceLabel   = flag.String("container-namespace-label", "namespace", `Label name to look for container namespaces`)
	ctrPodNameLabel     = flag.String("container-pod-name-label", "pod_name", `Label name to look for container pod names`)
	ctrNameLabel        = flag.String("container-name-label", "name", `Label name to look for container names`)
	vpaObjectNamespace  = flag.String("vpa-object-namespace", apiv1.NamespaceAll, "Namespace to search for VPA objects and pod stats. Empty means all namespaces will be used.")
)

// Aggregation configuration flags
var (
	memoryAggregationInterval      = flag.Duration("memory-aggregation-interval", model.DefaultMemoryAggregationInterval, `The length of a single interval, for which the peak memory usage is computed. Memory usage peaks are aggregated in multiples of this interval. In other words there is one memory usage sample per interval (the maximum usage over that interval)`)
	memoryAggregationIntervalCount = flag.Int64("memory-aggregation-interval-count", model.DefaultMemoryAggregationIntervalCount, `The number of consecutive memory-aggregation-intervals which make up the MemoryAggregationWindowLength which in turn is the period for memory usage aggregation by VPA. In other words, MemoryAggregationWindowLength = memory-aggregation-interval * memory-aggregation-interval-count.`)
	memoryHistogramDecayHalfLife   = flag.Duration("memory-histogram-decay-half-life", model.DefaultMemoryHistogramDecayHalfLife, `The amount of time it takes a historical memory usage sample to lose half of its weight. In other words, a fresh usage sample is twice as 'important' as one with age equal to the half life period.`)
	cpuHistogramDecayHalfLife      = flag.Duration("cpu-histogram-decay-half-life", model.DefaultCPUHistogramDecayHalfLife, `The amount of time it takes a historical CPU usage sample to lose half of its weight.`)
)

// Post processors flags
var (
	// CPU as integer to benefit for CPU management Static Policy ( https://kubernetes.io/docs/tasks/administer-cluster/cpu-management-policies/#static-policy )
	postProcessorCPUasInteger = flag.Bool("cpu-integer-post-processor-enabled", false, "Enable the cpu-integer recommendation post processor. The post processor will round up CPU recommendations to a whole CPU for pods which were opted in by setting an appropriate label on VPA object (experimental)")
)

func main() {
	klog.InitFlags(nil)
	kube_flag.InitFlags()
	klog.V(1).Infof("Vertical Pod Autoscaler %s Recommender: %v", common.VerticalPodAutoscalerVersion, recommenderName)

	config := common.CreateKubeConfigOrDie(*kubeconfig, float32(*kubeApiQps), int(*kubeApiBurst))

	model.InitializeAggregationsConfig(model.NewAggregationsConfig(*memoryAggregationInterval, *memoryAggregationIntervalCount, *memoryHistogramDecayHalfLife, *cpuHistogramDecayHalfLife))

	healthCheck := metrics.NewHealthCheck(*metricsFetcherInterval*5, true)
	metrics.Initialize(*address, healthCheck)
	metrics_recommender.Register()
	metrics_quality.Register()

	useCheckpoints := *storage != "prometheus"

	var postProcessors []routines.RecommendationPostProcessor
	if *postProcessorCPUasInteger {
		postProcessors = append(postProcessors, &routines.IntegerCPUPostProcessor{})
	}
	// CappingPostProcessor, should always come in the last position for post-processing
	postProcessors = append(postProcessors, &routines.CappingPostProcessor{})

	recommender := routines.NewRecommender(config, *checkpointsGCInterval, useCheckpoints, *vpaObjectNamespace, *recommenderName, postProcessors)

	promQueryTimeout, err := time.ParseDuration(*queryTimeout)
	if err != nil {
		klog.Fatalf("Could not parse --prometheus-query-timeout as a time.Duration: %v", err)
	}

	if useCheckpoints {
		recommender.GetClusterStateFeeder().InitFromCheckpoints()
	} else {
		config := history.PrometheusHistoryProviderConfig{
			Address:                *prometheusAddress,
			QueryTimeout:           promQueryTimeout,
			HistoryLength:          *historyLength,
			HistoryResolution:      *historyResolution,
			PodLabelPrefix:         *podLabelPrefix,
			PodLabelsMetricName:    *podLabelsMetricName,
			PodNamespaceLabel:      *podNamespaceLabel,
			PodNameLabel:           *podNameLabel,
			CtrNamespaceLabel:      *ctrNamespaceLabel,
			CtrPodNameLabel:        *ctrPodNameLabel,
			CtrNameLabel:           *ctrNameLabel,
			CadvisorMetricsJobName: *prometheusJobName,
			Namespace:              *vpaObjectNamespace,
		}
		provider, err := history.NewPrometheusHistoryProvider(config)
		if err != nil {
			klog.Fatalf("Could not initialize history provider: %v", err)
		}
		recommender.GetClusterStateFeeder().InitFromHistoryProvider(provider)
	}

	ticker := time.Tick(*metricsFetcherInterval)
	for range ticker {
		recommender.RunOnce()
		healthCheck.UpdateLastActivity()
	}
}
