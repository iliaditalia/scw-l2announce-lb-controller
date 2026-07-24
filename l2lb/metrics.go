/*
Copyright 2026 Iliad

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

package l2lb

import "github.com/prometheus/client_golang/prometheus"

var (
	metricReconciles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "scw_l2lb_reconciles_total",
		Help: "Number of service reconciliations, by result.",
	}, []string{"result"})

	metricMutations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "scw_l2lb_scaleway_mutations_total",
		Help: "Number of mutating Scaleway API calls, by operation.",
	}, []string{"op"})

	// metricDivergence is 1 while the IPAM attachment does not match the
	// current L2 lease holder's MAC. Alert on it staying 1: it means a Cilium
	// failover did not propagate to the Scaleway VPC gateway.
	metricDivergence = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scw_l2lb_divergence",
		Help: "1 when the lease holder MAC differs from the IPAM-attached MAC.",
	}, []string{"namespace", "name"})

	metricManagedServices = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "scw_l2lb_managed_services",
		Help: "Number of services currently opted in.",
	})
)

func init() {
	prometheus.MustRegister(metricReconciles, metricMutations, metricDivergence, metricManagedServices)
}
