package securityscan

import (
	"context"
	"fmt"
	"time"

	v1monitoringclient "github.com/prometheus-operator/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	"github.com/sirupsen/logrus"
	kubeapiext "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	detector "github.com/rancher/kubernetes-provider-detector"
	"github.com/rancher/wrangler/pkg/apply"
	"github.com/rancher/wrangler/pkg/crd"
	batchctl "github.com/rancher/wrangler/pkg/generated/controllers/batch"
	corectl "github.com/rancher/wrangler/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/pkg/start"

	"sync"

	"github.com/prometheus/client_golang/prometheus"

	cisoperatorapiv1 "github.com/rancher/cis-operator/pkg/apis/cis.cattle.io/v1"
	cisoperatorctl "github.com/rancher/cis-operator/pkg/generated/controllers/cis.cattle.io"
	"github.com/rancher/cis-operator/pkg/securityscan/scan"
)

type Controller struct {
	Namespace         string
	Name              string
	ClusterProvider   string
	KubernetesVersion string
	ImageConfig       *cisoperatorapiv1.ScanImageConfig

	kcs              *kubernetes.Clientset
	xcs              *kubeapiext.Clientset
	coreFactory      *corectl.Factory
	batchFactory     *batchctl.Factory
	cisFactory       *cisoperatorctl.Factory
	apply            apply.Apply
	monitoringClient v1monitoringclient.MonitoringV1Interface

	mu *sync.Mutex

	numTestsFailed   *prometheus.GaugeVec
	numScansComplete *prometheus.CounterVec
	numTestsSkipped  *prometheus.GaugeVec
	numTestsTotal    *prometheus.GaugeVec
	numTestsNA       *prometheus.GaugeVec
	numTestsPassed   *prometheus.GaugeVec
}

func NewController(ctx context.Context, cfg *rest.Config, namespace, name string, imgConfig *cisoperatorapiv1.ScanImageConfig) (ctl *Controller, err error) {
	if cfg == nil {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}
	ctl = &Controller{
		Namespace:   namespace,
		Name:        name,
		ImageConfig: imgConfig,
		mu:          &sync.Mutex{},
	}

	ctl.kcs, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	ctl.xcs, err = kubeapiext.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	ctl.ClusterProvider, err = detectClusterProvider(ctx, clientset)
	if err != nil {
		return nil, err
	}
	logrus.Infof("ClusterProvider detected %v", ctl.ClusterProvider)

	ctl.KubernetesVersion, err = detectKubernetesVersion(ctx, clientset)
	if err != nil {
		return nil, err
	}
	logrus.Infof("KubernetesVersion detected %v", ctl.KubernetesVersion)

	ctl.apply, err = apply.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	ctl.cisFactory, err = cisoperatorctl.NewFactoryFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("Error building securityscan NewFactoryFromConfig: %s", err.Error())
	}

	ctl.batchFactory, err = batchctl.NewFactoryFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("Error building batch NewFactoryFromConfig: %s", err.Error())
	}

	ctl.coreFactory, err = corectl.NewFactoryFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("Error building core NewFactoryFromConfig: %s", err.Error())
	}

	ctl.monitoringClient, err = v1monitoringclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("Error building v1 monitoring client from config: %s", err.Error())
	}

	err = initializeMetrics(ctl)
	if err != nil {
		return nil, fmt.Errorf("Error registering CIS Metrics: %s", err.Error())
	}
	return ctl, nil
}

func (c *Controller) Start(ctx context.Context, threads int, resync time.Duration) error {
	// register our handlers
	if err := c.handleJobs(ctx); err != nil {
		return err
	}
	if err := c.handlePods(ctx); err != nil {
		return err
	}
	if err := c.handleClusterScans(ctx); err != nil {
		return err
	}
	if err := c.handleScheduledClusterScans(ctx); err != nil {
		return err
	}
	if err := c.handleClusterScanMetrics(ctx); err != nil {
		return err
	}
	return start.All(ctx, threads, c.cisFactory, c.coreFactory, c.batchFactory)
}

func (c *Controller) registerCRD(ctx context.Context) error {
	factory := crd.NewFactoryFromClientGetter(c.xcs)

	var crds []crd.CRD
	for _, crdFn := range []func() (*crd.CRD, error){
		scan.ClusterScanCRD,
	} {
		crdef, err := crdFn()
		if err != nil {
			return err
		}
		crds = append(crds, *crdef)
	}
	return factory.BatchCreateCRDs(ctx, crds...).BatchWait()
}

func detectClusterProvider(ctx context.Context, k8sClient kubernetes.Interface) (string, error) {
	provider, err := detector.DetectProvider(ctx, k8sClient)
	if err != nil {
		return "", err
	}
	return provider, err
}

func detectKubernetesVersion(ctx context.Context, k8sClient kubernetes.Interface) (string, error) {
	v, err := k8sClient.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

func initializeMetrics(ctl *Controller) error {
	ctl.numTestsFailed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cis_scan_num_tests_fail",
			Help: "Number of test failed in the CIS scans, partioned by scan_name, scan_profile_name",
		},
		[]string{
			// scan_name will be set to "manual" for on-demand manual scans and the actual name set for the scheduled scans
			"scan_name",
			// name of the clusterScanProfile used for scanning
			"scan_profile_name",
		},
	)
	if err := prometheus.Register(ctl.numTestsFailed); err != nil {
		return err
	}

	ctl.numScansComplete = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cis_scan_num_scans_complete",
			Help: "Number of CIS clusterscans completed, partioned by scan_name, scan_profile_name",
		},
		[]string{
			// scan_name will be set to "manual" for on-demand manual scans and the actual name set for the scheduled scans
			"scan_name",
			// name of the clusterScanProfile used for scanning
			"scan_profile_name",
		},
	)
	if err := prometheus.Register(ctl.numScansComplete); err != nil {
		return err
	}

	ctl.numTestsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cis_scan_num_tests_total",
			Help: "Total Number of tests run in the CIS scans, partioned by scan_name, scan_profile_name",
		},
		[]string{
			// scan_name will be set to "manual" for on-demand manual scans and the actual name set for the scheduled scans
			"scan_name",
			// name of the clusterScanProfile used for scanning
			"scan_profile_name",
		},
	)
	if err := prometheus.Register(ctl.numTestsTotal); err != nil {
		return err
	}

	ctl.numTestsPassed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cis_scan_num_tests_pass",
			Help: "Number of tests passing in the CIS scans, partioned by scan_name, scan_profile_name",
		},
		[]string{
			// scan_name will be set to "manual" for on-demand manual scans and the actual name set for the scheduled scans
			"scan_name",
			// name of the clusterScanProfile used for scanning
			"scan_profile_name",
		},
	)
	if err := prometheus.Register(ctl.numTestsPassed); err != nil {
		return err
	}

	ctl.numTestsSkipped = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cis_scan_num_tests_skipped",
			Help: "Number of test skipped in the CIS scans, partioned by scan_name, scan_profile_name",
		},
		[]string{
			// scan_name will be set to "manual" for on-demand manual scans and the actual name set for the scheduled scans
			"scan_name",
			// name of the clusterScanProfile used for scanning
			"scan_profile_name",
		},
	)
	if err := prometheus.Register(ctl.numTestsSkipped); err != nil {
		return err
	}

	ctl.numTestsNA = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cis_scan_num_tests_na",
			Help: "Number of tests not applicable in the CIS scans, partioned by scan_name, scan_profile_name",
		},
		[]string{
			// scan_name will be set to "manual" for on-demand manual scans and the actual name set for the scheduled scans
			"scan_name",
			// name of the clusterScanProfile used for scanning
			"scan_profile_name",
		},
	)
	if err := prometheus.Register(ctl.numTestsNA); err != nil {
		return err
	}

	return nil
}
