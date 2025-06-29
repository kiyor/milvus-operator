package controllers

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/disk"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/zilliztech/milvus-operator/apis/milvus.io/v1beta1"
	"github.com/zilliztech/milvus-operator/pkg/helm"
	"github.com/zilliztech/milvus-operator/pkg/helm/values"
)

//go:generate mockgen -package=controllers -source=dependencies.go -destination=dependencies_mock.go HelmReconciler

const (
	Etcd     = "etcd"
	Minio    = "minio"
	Pulsar   = "pulsar"
	PulsarV3 = "pulsar-v3"
	Kafka    = "kafka"
	Tei      = "tei"
)

// HelmReconciler reconciles Helm releases
type HelmReconciler interface {
	NewHelmCfg(namespace string) *action.Configuration
	Reconcile(ctx context.Context, request helm.ChartRequest) error
	GetValues(namespace, release string) (map[string]interface{}, error)
}

type Chart = string
type Values = map[string]interface{}

// LocalHelmReconciler implements HelmReconciler at local
type LocalHelmReconciler struct {
	helmSettings *cli.EnvSettings
	logger       logr.Logger
	mgr          manager.Manager
}

func MustNewLocalHelmReconciler(helmSettings *cli.EnvSettings, logger logr.Logger, mgr manager.Manager) *LocalHelmReconciler {
	return &LocalHelmReconciler{
		helmSettings: helmSettings,
		logger:       logger,
		mgr:          mgr,
	}
}

func (l LocalHelmReconciler) NewHelmCfg(namespace string) *action.Configuration {
	cfg := new(action.Configuration)
	helmLogger := func(format string, v ...interface{}) {
		l.logger.Info(fmt.Sprintf(format, v...))
	}

	// cfg.Init will never return err, only panic if bad driver
	_ = cfg.Init(
		getRESTClientGetterFromClient(l.helmSettings, namespace, l.mgr),
		namespace,
		os.Getenv("HELM_DRIVER"),
		helmLogger,
	)

	return cfg
}

func getRESTClientGetterFromClient(env *cli.EnvSettings, namespace string, mgr manager.Manager) genericclioptions.RESTClientGetter {
	return &clientRESTClientGetter{
		namespace:  namespace,
		kubeConfig: env.KubeConfig,
		mgr:        mgr,
	}
}

type clientRESTClientGetter struct {
	namespace  string
	kubeConfig string
	mgr        manager.Manager
}

func (c *clientRESTClientGetter) ToRESTConfig() (*rest.Config, error) {
	// Get the config from the client
	return c.mgr.GetConfig(), nil
}

func (c *clientRESTClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	config, err := c.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	return disk.NewCachedDiscoveryClientForConfig(
		config,
		"",
		"",
		45*time.Minute,
	)
}

func (c *clientRESTClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	discoveryClient, err := c.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient), nil
}

func (c *clientRESTClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if c.kubeConfig != "" {
		loadingRules.ExplicitPath = c.kubeConfig
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{
			CurrentContext: "",
			Context: clientcmdapi.Context{
				Namespace: c.namespace,
			},
		})
}

func IsPulsarChartPath(chartPath string) bool {
	return chartPath == helm.GetChartPathByName(Pulsar)
}

// ReconcileHelm reconciles Helm releases
func (l LocalHelmReconciler) Reconcile(ctx context.Context, request helm.ChartRequest) error {
	cfg := l.NewHelmCfg(request.Namespace)

	exist, err := helm.ReleaseExist(cfg, request.ReleaseName)
	if err != nil {
		return err
	}

	if !exist {
		if request.Chart == helm.GetChartPathByName(Pulsar) {
			request.Values["initialize"] = true
		}
		l.logger.Info("helm install values", "values", request.Values)
		return helm.Install(cfg, request)
	}

	vals, err := helm.GetValues(cfg, request.ReleaseName)
	if err != nil {
		return err
	}

	status, err := helm.GetStatus(cfg, request.ReleaseName)
	if err != nil {
		return err
	}

	if request.Chart == helm.GetChartPathByName(Pulsar) {
		delete(vals, "initialize")
	}

	deepEqual := reflect.DeepEqual(vals, request.Values)
	needUpdate := helm.NeedUpdate(status)
	if deepEqual && !needUpdate {
		return nil
	}

	if request.Chart == helm.GetChartPathByName(Pulsar) {
		request.Values["initialize"] = false
	}

	l.logger.Info("update helm", "namespace", request.Namespace, "release", request.ReleaseName, "needUpdate", needUpdate, "deepEqual", deepEqual)
	if !deepEqual {
		l.logger.Info("update helm values", "old", vals, "new", request.Values)
	}

	return helm.Update(cfg, request)
}

func (l *LocalHelmReconciler) GetValues(namespace, release string) (map[string]interface{}, error) {
	cfg := l.NewHelmCfg(namespace)
	exist, err := helm.ReleaseExist(cfg, release)
	if err != nil {
		return nil, err
	}
	if !exist {
		return map[string]interface{}{}, nil
	}
	return helm.GetValues(cfg, release)
}

func (r *MilvusReconciler) ReconcileEtcd(ctx context.Context, mc v1beta1.Milvus) error {
	if mc.Spec.Dep.Etcd.External {
		return nil
	}
	request := helm.GetChartRequest(mc, values.DependencyKindEtcd, Etcd)

	return r.helmReconciler.Reconcile(ctx, request)
}

func (r *MilvusReconciler) ReconcileMsgStream(ctx context.Context, mc v1beta1.Milvus) error {
	switch mc.Spec.Dep.MsgStreamType {
	case v1beta1.MsgStreamTypeKafka:
		return r.ReconcileKafka(ctx, mc)
	case v1beta1.MsgStreamTypePulsar:
		return r.ReconcilePulsar(ctx, mc)
	default:
		// built in mq or custom mq, do nothing
		return nil
	}
}

func (r *MilvusReconciler) ReconcileKafka(ctx context.Context, mc v1beta1.Milvus) error {
	if mc.Spec.Dep.Kafka.External {
		return nil
	}
	request := helm.GetChartRequest(mc, values.DependencyKindKafka, Kafka)

	return r.helmReconciler.Reconcile(ctx, request)
}

func (r *MilvusReconciler) ReconcilePulsar(ctx context.Context, mc v1beta1.Milvus) error {
	if mc.Spec.Dep.Pulsar.External {
		return nil
	}
	request := helm.GetChartRequest(mc, values.DependencyKindPulsar, Pulsar)

	return r.helmReconciler.Reconcile(ctx, request)
}

func (r *MilvusReconciler) ReconcileMinio(ctx context.Context, mc v1beta1.Milvus) error {
	if mc.Spec.Dep.Storage.External {
		return nil
	}
	request := helm.GetChartRequest(mc, values.DependencyKindStorage, Minio)

	return r.helmReconciler.Reconcile(ctx, request)
}

func (r *MilvusReconciler) ReconcileTei(ctx context.Context, mc v1beta1.Milvus) error {
	if !mc.Spec.Dep.Tei.Enabled {
		return nil
	}
	request := helm.GetChartRequest(mc, values.DependencyKindTei, Tei)

	return r.helmReconciler.Reconcile(ctx, request)
}
